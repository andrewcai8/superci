import contextlib
import io
import os
import re
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import readset as R


class ImageOnly(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.image = os.path.join(self.dir.name, "image")
        for path, body in {"usr/bin/node": b"n" * 100, "usr/lib/libz.so": b"z" * 10, "etc/machine-id": b"m" * 33, "home/runner/.bashrc": b"b" * 7}.items():
            os.makedirs(os.path.join(self.image, os.path.dirname(path)), exist_ok=True)
            with open(os.path.join(self.image, path), "wb") as f:
                f.write(body)
        os.makedirs(os.path.join(self.image, "etc/ssh"))

    def tearDown(self):
        self.dir.cleanup()

    def file(self, name, text):
        p = os.path.join(self.dir.name, name)
        with open(p, "w") as f:
            f.write(text)
        return p

    def quiet(self, fn, *args):
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            return fn(*args)

    def test_build_keeps_only_the_images_files_as_the_image_has_them(self):
        a = self.file("a.tsv", "4096\t100\t/usr/bin/node\n4096\t9\t/etc/ssh/ssh_host_ed25519_key\n4096\t20\t/home/runner/_work/x\n")
        b = self.file("b.tsv", "4096\t100\t/usr/bin/node\n10\t10\t/usr/lib/libz.so\n4096\t11\t/usr/lib/libz.so.rewritten\n4096\t999\t/etc/ssh\n33\t33\t/etc/machine-id\n")
        c = self.file("c.tsv", "4096\t55\t/usr/lib/libz.so\n")
        out = os.path.join(self.dir.name, "out.list")
        self.quiet(R.build, self.image, out, [a, b])
        self.assertEqual(open(out).read(), "/usr/bin/node\n/usr/lib/libz.so\n")
        self.quiet(R.build, self.image, out, [c])
        self.assertEqual(open(out).read(), "", "a file a job rewrote to another size is not the image's")

    def test_build_keeps_a_path_one_recording_has_at_the_images_size_after_another_has_another(self):
        old = self.file("old.tsv", "4096\t55\t/usr/lib/libz.so\n")
        new = self.file("new.tsv", "10\t10\t/usr/lib/libz.so\n")
        out = os.path.join(self.dir.name, "out.list")
        self.quiet(R.build, self.image, out, [old, new])
        with open(out) as f:
            self.assertEqual(f.read(), "/usr/lib/libz.so\n")
        self.quiet(R.build, self.image, out, [new, old])
        with open(out) as f:
            self.assertEqual(f.read(), "/usr/lib/libz.so\n")

    def test_build_drops_an_image_file_under_every_tree_jobs_write(self):
        # Each tree build refuses is made an image file here, so only the refusal can drop it.
        paths = []
        for prefix in R.BOOT_WRITTEN_UNDER:
            path = prefix + "x" if prefix.endswith(("/", ".", "_")) else prefix + "/x"
            paths.append(path)
            os.makedirs(os.path.join(self.image, os.path.dirname(path).lstrip("/")), exist_ok=True)
            with open(os.path.join(self.image, path.lstrip("/")), "wb") as f:
                f.write(b"i" * 5)
        for path in paths:
            self.assertTrue(R.image_file(self.image, path, 5), f"{path} is an image file, so only its tree can drop it")
        a = self.file("a.tsv", "".join(f"4096\t5\t{p}\n" for p in paths) + "4096\t100\t/usr/bin/node\n")
        out = os.path.join(self.dir.name, "out.list")
        self.quiet(R.build, self.image, out, [a])
        with open(out) as f:
            kept = f.read()
        self.assertEqual(kept, "/usr/bin/node\n", f"kept a path under {[p for p in paths if p in kept]}")

    def test_build_drops_each_file_every_boot_writes(self):
        paths = []
        for path in R.BOOT_WRITTEN:
            paths.append(path)
            os.makedirs(os.path.join(self.image, os.path.dirname(path).lstrip("/")), exist_ok=True)
            with open(os.path.join(self.image, path.lstrip("/")), "wb") as f:
                f.write(b"b" * 5)
        a = self.file("a.tsv", "".join(f"4096\t5\t{p}\n" for p in paths) + "4096\t100\t/usr/bin/node\n")
        out = os.path.join(self.dir.name, "out.list")
        self.quiet(R.build, self.image, out, [a])
        with open(out) as f:
            self.assertEqual(f.read(), "/usr/bin/node\n")

    def test_the_dropped_trees_are_the_ones_readsets_test_go_refuses(self):
        # readset.py drops what readsets_test.go refuses in a committed list; the two lists must not drift apart.
        with open(os.path.join(os.path.dirname(__file__), "..", "..", "readsets_test.go")) as f:
            go = f.read()

        def go_strings(name):
            m = re.search(r"var %s = \[\]string\{(.*?)\}" % name, go, re.S)
            self.assertIsNotNone(m, f"readsets_test.go has no `var {name}`")
            return re.findall(r'"([^"]*)"', m.group(1))

        self.assertEqual(sorted(R.BOOT_WRITTEN_UNDER), sorted(go_strings("writtenOnMachines")))
        self.assertEqual(sorted(R.BOOT_WRITTEN), sorted(go_strings("bootWritten")))
        self.assertGreater(len(R.BOOT_WRITTEN_UNDER), 0)

    def test_check_prints_how_many_paths_it_checked(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            R.check(self.image, self.file("two.list", "/usr/bin/node\n/etc/ssh/ssh_host_ed25519_key\n"))
        self.assertIn("1 of 2 paths are not image files", out.getvalue())

    def test_check_refuses_a_path_the_image_lacks(self):
        self.assertEqual(self.quiet(R.check, self.image, self.file("ok.list", "/usr/bin/node\n/usr/lib/libz.so\n")), 0)
        self.assertEqual(self.quiet(R.check, self.image, self.file("key.list", "/usr/bin/node\n/etc/ssh/ssh_host_ed25519_key\n")), 1)
        self.assertEqual(self.quiet(R.check, self.image, self.file("dir.list", "/etc/ssh\n")), 1, "a directory is not a file to read")

    def test_check_refuses_a_path_that_leaves_the_image(self):
        outside = os.path.join(self.dir.name, "host")
        os.makedirs(outside)
        with open(os.path.join(outside, "secret"), "w") as f:
            f.write("x")
        os.symlink(outside, os.path.join(self.image, "hostlink"))
        os.symlink("usr/lib", os.path.join(self.image, "lib"))
        self.assertEqual(self.quiet(R.check, self.image, self.file("dotdot.list", "/../host/secret\n")), 1)
        self.assertEqual(self.quiet(R.check, self.image, self.file("link.list", "/hostlink/secret\n")), 1)
        self.assertEqual(self.quiet(R.check, self.image, self.file("inside.list", "/lib/libz.so\n")), 0, "a link inside the image stays in it")


if __name__ == "__main__":
    unittest.main()
