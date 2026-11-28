# readset

Records which of the machine image's files a size's jobs read, so its machines read only those while they wait
([Read set](../../README.md#hydration)). Python 3 standard library. It changes nothing on the machine it runs on but the
file it writes.

```
readset.py resident OUT                       on a lab machine that has just run one job: the page cache's files
readset.py build --image MNT OUT RESIDENT...  the union, kept only where the image has the file at the same size
readset.py check --image MNT LIST             exit 1 unless every path of LIST is a regular file of the image; prints "N of M"
python3 -m unittest pull/tools/readset/test_readset.py
```

## Recording a list

Run everything in a lab: an account-243891825325 instance tagged for the lab, never a listener, an identity host or this
host. Terminate every instance at the end.

1. Launch one machine of the size from the image the listener boots (`findImage` in `fleet.go`), with the size's root
   volume. Apply boot.sh's machine settings but none of its reads: no hydration, pre-read or warm.
2. Run one real job of the size on it, through a repository runner on a lab label from a load branch, and nothing else.
3. `readset.py resident` after the job. Repeat on fresh machines for other legs of the job and the other jobs of the
   size. Each run adds the files its leg reads, and the Nov 27 list for superci-8cpu took ten (two solo legs, eight
   packed legs). The Nov 28 list for superci-4cpu took all 24 packed 4 vCPU unit-tests legs of the queue fleet, one on a machine alone first, then
   the rest in batches of six, and then leg 3 again, because the first recording of leg 3 held only 62,643 resident files. Built against
   the current image (3,002 image files after leg 1, 3,012 after two legs, 3,073 after ten and 3,095 after all 24), the legs' union is
   3,108 image files with the second leg 3. It then took four runs of test-go (one job, four fresh machines, on main 30cc3a4): the first added 198 image files
   (cgo's gcc, as and ld, and the C headers they read), the second and third none, and the fourth, on the current image, 66 (Chrome's files, which
   the current image has at another size, and kernel 7.0.0-1018's modules). The list holds 3,372. Twenty-three legs and three test-go runs were recorded
   on RunsOn 20261117081256, which RunsOn has since deleted; leg 3 and the fourth test-go run were recorded on 20261120075252,
   the build the listener now boots, and `build` and `check` ran against its snapshot. A file the newer image changed (libpq, Chrome) is in the list
   only where a machine of the newer image read it.
4. Make a volume from the image's root snapshot (`create-volume --snapshot-id` takes the image's public snapshot; the EBS direct APIs refuse it). Attach
   it to a lab machine and mount it read-only (`-o ro,noload`).
5. `readset.py build --image MNT readsets/<label>.list` on the resident files, then `readset.py check` on the result.
   Commit the list. `readsets_test.go` holds every committed list to sorted, absolute paths outside the trees jobs and
   boot write. Run both as an ordinary user, not root: a path the user cannot reach (docker's state under /var/lib/docker,
   sudoers.d, 353 paths on Nov 28) is not a job's read, and root's `build` kept all of them.

## Why a list holds only image files

A machine reads every listed file and discards it, so a path is all a list carries from the machines it was recorded on.
A recording machine also holds files its boot and its job wrote: ssh host keys, cloud-init's instance data, the job's
checkout. On Nov 27 the union of ten recordings held 151 such paths and 11 files rewritten to another size. `build` keeps
a path only when the snapshot's volume has it as a regular file of the recorded size, reached without leaving the
volume (no `..`, no directory link out of it), and drops the few files every boot rewrites (`/etc/hosts`,
`/etc/machine-id`, dhcpcd's leases). That left 3,041 of 3,207. `check` on the list found 0 that were not image files, and
a planted `/etc/ssh/ssh_host_ed25519_key` made it exit 1.

## When the image or the jobs change

A path the new image lacks costs one failed open. A file a job newly reads is read cold until the list is recorded again.
That is as slow as a machine with no pre-read, never wrong. `readset.done=full:missing` says the list names paths the
image lacks: record it again then, or when a size's jobs' first-exec times (`gate/spawn.py`) drift from Blacksmith. An
empty list reads nothing, and that machine pre-reads as before.

## Turning a list off

Delete `readsets/<label>.list` and, in each region's cache bucket, `bin/warm/<label>.list`. A release only uploads, so the
bucket keeps a deleted list (and `verify-release.sh` then fails on it) until it is deleted there. Machines launched
afterwards pre-read as before.
