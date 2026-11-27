#!/usr/bin/env python3
"""readset: records which of the machine image's files a size's jobs read, so a machine reads only those while it waits.

  readset.py resident OUT                      on a machine that has just run one job: every regular file of the root
                                               filesystem with pages in the page cache, as <resident bytes>\\t<size>\\t<path>
  readset.py build --image MNT OUT RESIDENT... the union of resident files that are image files: a path is kept only
                                               when it is a regular file of the same size in MNT, a volume made from the
                                               image's snapshot and mounted read-only
  readset.py check --image MNT LIST            exits 1 unless every path of LIST is a regular file in MNT

A read set holds only paths of the image, never a file a job wrote: `build` keeps nothing the snapshot lacks or that a
job rewrote to another size, and `check` proves it of a list as committed. A machine only reads the listed files, so a
path is all a list carries from the machines it was recorded on.
"""
import ctypes
import os
import stat
import sys

PAGE = os.sysconf("SC_PAGE_SIZE")
SKIP_TOP = {"proc", "sys", "dev", "run", "tmp"}
# Every boot writes these afresh, so the image's copy is never the one a job reads (readsets_test.go holds the same).
BOOT_WRITTEN = ("/etc/hosts", "/etc/hostname", "/etc/machine-id")
BOOT_WRITTEN_UNDER = ("/var/lib/dhcp", "/etc/ssh/ssh_host_")


def resident_bytes(libc, path, size):
    """Bytes of the file in the page cache (mmap and mincore), or 0."""
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOATIME", 0))
    try:
        addr = libc.mmap(None, size, 1, 1, fd, 0)  # PROT_READ, MAP_SHARED
    finally:
        os.close(fd)
    if addr in (None, ctypes.c_void_p(-1).value):
        return 0
    pages = (size + PAGE - 1) // PAGE
    vec = (ctypes.c_ubyte * pages)()
    rc = libc.mincore(addr, size, vec)
    libc.munmap(addr, size)
    return 0 if rc else min(size, sum(b & 1 for b in bytes(vec)) * PAGE)


def same_device(path, dev):
    try:
        return os.lstat(path).st_dev == dev
    except OSError:
        return False


def resident(out):
    libc = ctypes.CDLL("libc.so.6", use_errno=True)
    libc.mmap.restype = ctypes.c_void_p
    libc.mmap.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_long]
    libc.munmap.argtypes = [ctypes.c_void_p, ctypes.c_size_t]
    libc.mincore.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.POINTER(ctypes.c_ubyte)]
    dev = os.stat("/").st_dev
    n = total = 0
    with open(out, "w") as w:
        for root, dirs, files in os.walk("/"):
            dirs[:] = [d for d in dirs if not (root == "/" and d in SKIP_TOP) and same_device(os.path.join(root, d), dev)]
            for f in files:
                p = os.path.join(root, f)
                if not p.isprintable():
                    continue
                try:
                    st = os.lstat(p)
                    if not stat.S_ISREG(st.st_mode) or st.st_size == 0:
                        continue
                    b = resident_bytes(libc, p, st.st_size)
                except OSError:
                    continue
                if b:
                    n, total = n + 1, total + b
                    w.write(f"{b}\t{st.st_size}\t{p}\n")
    print(f"resident: {n} files, {total / 2**20:.1f} MiB")


def image_file(image, path, size=None):
    """The path is a regular file of the image mounted at image, reached without leaving it: no .. and no directory link out."""
    if not path.startswith("/") or os.path.normpath(path) != path:
        return False
    full = os.path.join(image, path.lstrip("/"))
    root = os.path.realpath(image)
    if os.path.commonpath([root, os.path.realpath(os.path.dirname(full))]) != root:
        return False
    try:
        st = os.lstat(full)
    except OSError:
        return False
    return stat.S_ISREG(st.st_mode) and (size is None or st.st_size == size)


def build(image, out, residents):
    """Keeps a recorded path only when the image has it as a regular file of the size the machine read."""
    seen, kept, dropped = set(), [], 0
    for r in residents:
        for line in open(r):
            fields = line.rstrip("\n").split("\t", 2)
            if len(fields) != 3 or not fields[2].startswith("/") or fields[2] in seen:
                continue
            seen.add(fields[2])
            if fields[2] not in BOOT_WRITTEN and not fields[2].startswith(BOOT_WRITTEN_UNDER) and image_file(image, fields[2], int(fields[1])):
                kept.append(fields[2])
            else:
                dropped += 1
    with open(out, "w") as w:
        w.writelines(p + "\n" for p in sorted(kept))
    print(f"build: {len(kept)} image files kept, {dropped} dropped (not in the image, or not as the image has them)")


def check(image, listing):
    bad = [p for p in open(listing).read().split("\n") if p and not image_file(image, p)]
    for p in bad[:20]:
        print(f"not an image file: {p}", file=sys.stderr)
    print(f"check: {len(bad)} of the list's paths are not image files")
    return 1 if bad else 0


def main(argv):
    if len(argv) == 2 and argv[0] == "resident":
        resident(argv[1])
        return 0
    if len(argv) >= 4 and argv[0] in ("build", "check") and argv[1] == "--image":
        if argv[0] == "build" and len(argv) >= 5:
            build(argv[2], argv[3], argv[4:])
            return 0
        if argv[0] == "check" and len(argv) == 4:
            return check(argv[2], argv[3])
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
