#!/bin/bash
# superci-image: setup
# What image.sh bakes into the machine image, as root on its builder (a machine of the current pin): the changes that let a
# machine of this image hibernate, and nothing that costs a machine that does not. The sha256 of this file is part of the
# image's build id (image.sh), so an edit here makes a new image and never changes one that exists.
#
# ec2-hibinit-agent ships in the image but disabled. Enabled, it asks the metadata service at each boot whether the
# instance was launched with HibernationOptions.Configured, and exits at once when it was not: a machine launched as the
# fleet's are (no hibernation) makes no swap file, takes no disk and adds only that call. On a machine launched with it, the
# agent makes /swap-hibinit (the size of RAM), writes resume= and resume_offset= into GRUB and leaves swap off; ACPI's sleep
# button (what EC2 sends to hibernate) runs /etc/acpi/actions/sleep.sh, which turns the swap file on and hibernates. No
# reboot is needed between a launch and the first hibernate, and no fstab line; image.sh's verification launches, hibernates
# and resumes exactly so.
#
# The agent refuses ("Insufficient disk space") while the root file system is smaller than RAM, and the image's file system is
# its 30 GB until boot.sh grows it, which comes after the agent. A pre-start step on the agent's own unit therefore grows the
# root first, and only when the metadata service says hibernation is configured: a machine without it runs one metadata call
# there and nothing else (the Nov 28 lab met the refusal and rebooted its machines to get past it).
#
# logind: its default for the sleep key is suspend, which an EC2 machine cannot do; with it left there the machine sat in
# `stopping` for over 3 minutes after StopInstances Hibernate=true. HandleSuspendKey=hibernate makes the key hibernate.
set -euo pipefail
install -d -m 755 /usr/local/sbin
cat > /usr/local/sbin/superci-hibinit-prepare << 'PREP'
#!/bin/bash
# hibinit-agent's ExecStartPre (image.sh's setup): on an instance launched with hibernation, grow the root so the agent finds
# room for a swap file the size of RAM; on any other, do nothing.
imds=http://169.254.169.254/latest
token=$(curl -fsS -m 3 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' "$imds/api/token") || exit 0
[ "$(curl -fsS -m 3 -H "X-aws-ec2-metadata-token: $token" "$imds/meta-data/hibernation/configured")" = true ] || exit 0
root=$(findmnt -no SOURCE /)
growpart "/dev/$(lsblk -no PKNAME "$root")" "${root##*[!0-9]}" || true # exits 1 when there is nothing to grow
resize2fs "$root"
PREP
chmod 755 /usr/local/sbin/superci-hibinit-prepare
install -d -m 755 /etc/systemd/system/hibinit-agent.service.d
printf '[Service]\nExecStartPre=/usr/local/sbin/superci-hibinit-prepare\n' > /etc/systemd/system/hibinit-agent.service.d/50-superci-grow-root.conf
chmod 644 /etc/systemd/system/hibinit-agent.service.d/50-superci-grow-root.conf
systemctl enable hibinit-agent.service
install -d -m 755 /etc/systemd/logind.conf.d
printf '[Login]\nHandleSuspendKey=hibernate\n' > /etc/systemd/logind.conf.d/50-superci-hibernate.conf
chmod 644 /etc/systemd/logind.conf.d/50-superci-hibernate.conf
systemctl daemon-reload
# What it did, so a builder that did not take the change stops the build here.
[ "$(systemctl is-enabled hibinit-agent.service)" = enabled ] || { echo "setup: hibinit-agent is not enabled" >&2; exit 1; }
[ -f /etc/hibinit-config.cfg ] || { echo "setup: the image has no /etc/hibinit-config.cfg, so the agent would never run" >&2; exit 1; }
systemctl show hibinit-agent.service -p ExecStartPre --value | grep -q superci-hibinit-prepare || { echo "setup: the agent does not run the root-growing step" >&2; exit 1; }
grep -qx 'HandleSuspendKey=hibernate' /etc/systemd/logind.conf.d/50-superci-hibernate.conf
bash -n /usr/local/sbin/superci-hibinit-prepare
echo "setup: hibinit-agent enabled (it grows the root first when hibernation is configured), logind HandleSuspendKey=hibernate"
