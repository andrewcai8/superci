#!/bin/bash
# Times the root volume's writes at several gp3 settings, each on its own lab instance (the superci image, the size's instance type and root size), and
# prints the table. Each instance is tagged purpose=superci-disk-lab and terminated at the end, or when this script stops. It changes nothing but those
# instances and their volumes; it reads the console of each, so it needs no ssh and no credential on the machine.
#
#   pull/tools/disk/lab.sh [TYPE [ROOT_GB]] [-- MBPS:IOPS ...]      default: m8azn.xlarge 60 -- 250:3000 500:3000 750:3000 1000:4000 1000:16000
set -uo pipefail
A="aws --profile ci-runners --region us-east-1"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
TYPE=m8azn.xlarge GB=60
[ $# -gt 0 ] && [ "$1" != -- ] && { TYPE=$1; shift; }
[ $# -gt 0 ] && [ "$1" != -- ] && { GB=$1; shift; }
[ "${1:-}" = -- ] && shift
CFGS=("$@"); [ ${#CFGS[@]} -gt 0 ] || CFGS=(250:3000 500:3000 750:3000 1000:4000 1000:16000)
SG=$($A ec2 describe-security-groups --filters Name=group-name,Values=superci-pull-listener --query 'SecurityGroups[0].GroupId' --output text)
AMI=$($A ec2 describe-images --owners 135269210855 --filters "Name=name,Values=runs-on-v2.2-ubuntu24-full-x64-*" --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)
ids=()
trap '[ ${#ids[@]} -eq 0 ] || $A ec2 terminate-instances --instance-ids "${ids[@]}" --query "TerminatingInstances[].InstanceId" --output text; echo terminated' EXIT
for c in "${CFGS[@]}"; do
  mbps=${c%%:*} iops=${c##*:}
  ud=$(sed "s/@CFG@/${mbps}MiBps-${iops}iops/" "$here/bench.sh")
  id=$($A ec2 run-instances --image-id "$AMI" --instance-type "$TYPE" --security-group-ids "$SG" --instance-initiated-shutdown-behavior terminate \
    --metadata-options HttpTokens=required,HttpPutResponseHopLimit=1 \
    --block-device-mappings "DeviceName=/dev/sda1,Ebs={VolumeSize=$GB,VolumeType=gp3,Iops=$iops,Throughput=$mbps,DeleteOnTermination=true}" \
    --tag-specifications "ResourceType=instance,Tags=[{Key=purpose,Value=superci-disk-lab},{Key=Name,Value=superci-disk-lab-$mbps-$iops}]" "ResourceType=volume,Tags=[{Key=purpose,Value=superci-disk-lab}]" \
    --user-data "$ud" --query 'Instances[0].InstanceId' --output text) || exit 1
  ids+=("$id"); echo "$c on $id"
done
sleep 150
for n in "${!CFGS[@]}"; do
  out=""
  for _ in 1 2 3 4 5 6; do
    out=$($A ec2 get-console-output --instance-id "${ids[$n]}" --latest --output text --query Output | grep -a "^LAB:" | sort -u)
    grep -aq "LAB: end" <<< "$out" && break; sleep 20
  done
  echo "== ${CFGS[$n]} (MiB/s:IOPS) on $TYPE"; sed 's/^LAB: //' <<< "$out" | grep -av "^begin\|^root"
done
