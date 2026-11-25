#!/bin/bash
# A throwaway second listener, to try a build on real GitHub jobs without touching production's: its own instance, its own
# deployment (runner group, machines' tags) and, through the override's "only", one label of its own, so no job asking for a
# production label can reach it. It uses the role superci-pull-dev, the machines' own role and runner security groups, and
# the log group /superci-pull/listener (its stream is its instance id). Its override names the deployment, the
# repositories and the one size:
#
#   {"deployment":"superci-pull-NAME","repositories":["megpt-mono"],"only":["superci-96cpu"],
#    "sizes":{"superci-96cpu":{"cpu":2,"split":1,"max":20,"disk_gb":40,"types":["c7i.large", ...]}}}
#
#   pull/deploy/dev.sh up NAME <binary> <override.json>   starts the instance and the listener
#   pull/deploy/dev.sh ssh NAME [command]                 a shell on it (a key made for the call: EC2 Instance Connect)
#   pull/deploy/dev.sh down NAME                           deletes its scale sets and terminates it
set -euo pipefail
A="aws --profile ci-runners --region us-east-1"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
cmd=${1:?up|ssh|down}
NAME=${2:?name}
here=$(cd "$(dirname "$0")" && pwd)

instance() { $A ec2 describe-instances --filters Name=tag:Name,Values=superci-pull-dev-$NAME Name=instance-state-name,Values=pending,running \
  --query 'Reservations[0].Instances[0].[InstanceId,Placement.AvailabilityZone,PublicIpAddress]' --output text; }

ssh_run() {
  local id zone ip tmp
  read -r id zone ip < <(instance)
  [ "$id" != None ] || { echo "no instance superci-pull-dev-$NAME" >&2; return 1; }
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  ssh-keygen -q -t ed25519 -N '' -f "$tmp/key"
  $A ec2-instance-connect send-ssh-public-key --instance-id "$id" --availability-zone "$zone" --instance-os-user ec2-user --ssh-public-key "file://$tmp/key.pub" > /dev/null
  ssh -q -i "$tmp/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$tmp/known" -o ConnectTimeout=10 "ec2-user@$ip" "$@"
}

case $cmd in
  up)
    BIN=${3:?binary}; OVERRIDE=${4:?override.json}
    [ "$(instance | cut -f1)" = None ] || { echo "superci-pull-dev-$NAME is already running" >&2; exit 1; }
    AMI=$($A ssm get-parameter --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 --query Parameter.Value --output text)
    SG=$($A ec2 describe-security-groups --filters Name=group-name,Values=superci-pull-listener --query 'SecurityGroups[0].GroupId' --output text)
    RUNNERS=$(for r in us-east-1 us-east-2; do echo -n "$r=$($A ec2 describe-security-groups --region $r --filters Name=group-name,Values=superci-pull-runner --query 'SecurityGroups[0].GroupId' --output text),"; done)
    ID=$($A ec2 run-instances --image-id "$AMI" --instance-type c7g.large --security-group-ids "$SG" --iam-instance-profile Name=superci-pull-dev \
      --associate-public-ip-address --instance-initiated-shutdown-behavior terminate --metadata-options HttpTokens=required,HttpPutResponseHopLimit=1 \
      --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=30,VolumeType=gp3,DeleteOnTermination=true}' \
      --tag-specifications "ResourceType=instance,Tags=[{Key=purpose,Value=superci-pull-dev},{Key=Name,Value=superci-pull-dev-$NAME}]" "ResourceType=volume,Tags=[{Key=purpose,Value=superci-pull-dev}]" \
      --user-data "#!/bin/bash
( sleep 14400; poweroff ) &" --query 'Instances[0].InstanceId' --output text)
    echo "instance $ID"
    $A ec2 wait instance-running --instance-ids "$ID"
    until ssh_run true 2> /dev/null; do sleep 5; done
    IP=$($A ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    ssh_run 'sudo mkdir -p /etc/superci-pull /var/log/superci-pull && sudo tee /usr/local/bin/superci-pull > /dev/null && sudo chmod 755 /usr/local/bin/superci-pull' < "$BIN"
    ssh_run 'sudo tee /etc/superci-pull/config.json > /dev/null' < "$OVERRIDE"
    ssh_run 'sudo /usr/local/bin/superci-pull -validate -config /etc/superci-pull/config.json'
    ssh_run "sudo tee /etc/superci-pull.env > /dev/null" <<< "FLAGS=-public-ip=$IP -security-groups=${RUNNERS%,} -tls-param=/superci-pull-dev/tls -spend-param=/superci-pull-dev/spend"
    ssh_run 'sudo tee /etc/systemd/system/superci-pull.service > /dev/null' < "$here/superci-pull.service"
    ssh_run 'sudo tee /etc/superci-pull-agent.json > /dev/null' < "$here/cloudwatch-agent.json"
    ssh_run 'sudo systemctl daemon-reload && sudo systemctl enable --now superci-pull.service'
    ssh_run 'sudo dnf install -y -q amazon-cloudwatch-agent > /dev/null && sudo /opt/aws/amazon-cloudwatch-agent/bin/amazon-cloudwatch-agent-ctl -a fetch-config -m ec2 -s -c file:/etc/superci-pull-agent.json > /dev/null'
    echo "listener up at $IP (instance $ID); events in /superci-pull/listener, stream $ID"
    ;;
  ssh)
    shift 2
    ssh_run "$@"
    ;;
  down)
    ssh_run 'sudo systemctl stop superci-pull; sudo /usr/local/bin/superci-pull -delete-scale-sets -config /etc/superci-pull/config.json $(sed -n "s/^FLAGS=//p" /etc/superci-pull.env)' || true
    read -r id _ _ < <(instance)
    [ "$id" = None ] || $A ec2 terminate-instances --instance-ids "$id" > /dev/null
    echo "terminated $id"
    ;;
  *) echo "up|ssh|down" >&2; exit 1 ;;
esac
