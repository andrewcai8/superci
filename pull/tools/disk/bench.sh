#!/bin/bash
# The disk benchmark pull/tools/disk/lab.sh gives a lab instance as its user data. It runs ON that instance, never on the host that launched it:
# it grows the root and sets what boot.sh sets before a job, times 1 GiB and 4 GiB writes with fdatasync and a 20 s random 4 KiB write, prints
# LAB: lines to the console, and powers off 15 minutes later (the instance is shut down with terminate).
CFG=@CFG@
say() { echo "LAB: $*" | tee /dev/console; }
( sleep 1200; poweroff ) > /dev/null 2>&1 &
sleep 45
say "begin cfg=$CFG type=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)" http://169.254.169.254/latest/meta-data/instance-type) kernel=$(uname -r) nproc=$(nproc) mem_kb=$(awk '/MemTotal/{print $2}' /proc/meminfo)"
# what boot.sh does to the root before a job: the sysctl, noauto_da_alloc, and growing the root to the volume
printf '%s\n' vm.dirty_ratio=50 > /etc/sysctl.d/99-lab.conf; sysctl -eqp /etc/sysctl.d/99-lab.conf
root=$(findmnt -no SOURCE /)
mount -o remount,noauto_da_alloc /
growpart "/dev/$(lsblk -no PKNAME "$root")" "${root##*[!0-9]}" > /dev/null 2>&1 && resize2fs "$root" > /dev/null 2>&1
say "root=$root size=$(lsblk -bndo SIZE "$root") df=$(df -BG --output=size,avail / | tail -1 | tr -s ' ')"
sleep 20
dd1() { local t0 t1; sync; t0=$(date +%s.%N); dd if=/dev/zero of=/var/tmp/w bs=1M count=$1 conv=fdatasync status=none; t1=$(date +%s.%N); rm -f /var/tmp/w; sync; awk -v a="$t1" -v b="$t0" 'BEGIN{printf "%.2f", a-b}'; }
for i in 1 2 3; do say "write 1GiB fdatasync run $i: $(dd1 1024) s"; done
say "write 4GiB fdatasync: $(dd1 4096) s"
fio --name=rw --filename=/var/tmp/f --size=2G --rw=randwrite --bs=4k --iodepth=32 --ioengine=libaio --direct=1 --runtime=20 --time_based --group_reporting 2>&1 | grep -E "IOPS=|WRITE:" | sed 's/^ *//' | while read -r l; do say "randwrite4k: $l"; done
rm -f /var/tmp/f
say "end cfg=$CFG"
sleep 900
poweroff
