import contextlib
import io
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import readset as R


class ImageOnly(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.image = os.path.join(self.dir.name, "image")
        for path, body in {"usr/bin/node": b"n" * 100, "usr/lib/libz.so": b"z" * 10, "etc/machine-id": b"m" * 33}.items():
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
