"""OCI metadata checks use independently constructed index/config fixtures."""

import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from images import inspect_oci


class ImageTests(unittest.TestCase):
    def archive(self, folder, revision="commit", architectures=("amd64", "arm64"), corrupt=False):
        blobs = {}

        def blob(value):
            data = json.dumps(value).encode()
            digest = hashlib.sha256(data).hexdigest()
            blobs[f"blobs/sha256/{digest}"] = data
            return {"digest": "sha256:" + digest, "size": len(data)}

        manifests = []
        for arch in architectures:
            config = blob({"os": "linux", "architecture": arch, "config": {"User": "65532:65532", "Labels": {"org.opencontainers.image.version": "0.1.0-alpha.2", "org.opencontainers.image.revision": revision}}})
            item = blob({"schemaVersion": 2, "config": config, "layers": []})
            item["mediaType"] = "application/vnd.oci.image.manifest.v1+json"
            manifests.append(item)
        index = blob({"schemaVersion": 2, "manifests": manifests})
        index["mediaType"] = "application/vnd.oci.image.index.v1+json"
        blobs["index.json"] = json.dumps({"schemaVersion": 2, "manifests": [index]}).encode()
        if corrupt:
            name = next(name for name in blobs if name.startswith("blobs/"))
            blobs[name] = b"altered configuration"
        path = folder / "image.tar"
        with tarfile.open(path, "w") as archive:
            for name, data in blobs.items():
                header = tarfile.TarInfo(name)
                header.size = len(data)
                archive.addfile(header, io.BytesIO(data))
        return path

    def test_nested_index_matches_both_platforms_and_release_identity(self):
        with tempfile.TemporaryDirectory() as folder:
            image = self.archive(Path(folder))
            actual = inspect_oci(image, "0.1.0-alpha.2", "commit")
            with tarfile.open(image) as archive:
                expected = json.load(archive.extractfile("index.json"))["manifests"][0]["digest"]
            self.assertEqual(actual, {"platforms": ["linux/amd64", "linux/arm64"], "digest": expected})

    def test_wrong_revision_cannot_join_the_candidate_manifest(self):
        with tempfile.TemporaryDirectory() as folder:
            image = self.archive(Path(folder), revision="different-source")
            with self.assertRaisesRegex(ValueError, "identity"):
                inspect_oci(image, "0.1.0-alpha.2", "commit")

    def test_missing_or_duplicate_architecture_is_rejected(self):
        with tempfile.TemporaryDirectory() as folder:
            for architectures in [("amd64",), ("amd64", "amd64", "arm64")]:
                image = self.archive(Path(folder), architectures=architectures)
                with self.assertRaisesRegex(ValueError, "platform"):
                    inspect_oci(image, "0.1.0-alpha.2", "commit")

    def test_corrupt_blob_is_rejected_before_metadata_is_trusted(self):
        with tempfile.TemporaryDirectory() as folder:
            image = self.archive(Path(folder), corrupt=True)
            with self.assertRaisesRegex(ValueError, "digest"):
                inspect_oci(image, "0.1.0-alpha.2", "commit")


if __name__ == "__main__":
    unittest.main()
