"""Verify OCI candidate identity/platforms and add image archives to checksums."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import tarfile

from build import write_manifest
from smoke import read_candidate


def inspect_oci(path, version, revision):
    platforms = []
    with tarfile.open(path) as archive:
        def document(descriptor):
            digest = descriptor["digest"]
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                raise ValueError("invalid OCI digest")
            member = archive.getmember("blobs/sha256/" + digest.split(":", 1)[1])
            if not member.isfile():
                raise ValueError("OCI metadata blob is not an ordinary file")
            data = archive.extractfile(member).read()
            if hashlib.sha256(data).hexdigest() != digest.split(":", 1)[1] or len(data) != descriptor["size"]:
                raise ValueError("OCI metadata digest mismatch")
            return json.loads(data)

        def visit(descriptor, depth=0):
            if depth > 4:
                raise ValueError("OCI index nesting exceeds candidate limits")
            if descriptor.get("annotations", {}).get("vnd.docker.reference.type") == "attestation-manifest":
                return
            body = document(descriptor)
            if "manifests" in body:
                for child in body["manifests"]:
                    visit(child, depth + 1)
                return
            config = document(body["config"])
            selected = f'{config["os"]}/{config["architecture"]}'
            declared = descriptor.get("platform")
            if declared and (declared.get("os") != config["os"] or declared.get("architecture") != config["architecture"]):
                raise ValueError("OCI platform descriptor disagrees with image configuration")
            if selected not in {"linux/amd64", "linux/arm64"} or selected in platforms:
                raise ValueError("unsupported or duplicate OCI platform")
            metadata = config["config"]
            labels = metadata.get("Labels", {})
            if labels.get("org.opencontainers.image.version") != version or labels.get("org.opencontainers.image.revision") != revision:
                raise ValueError("OCI image identity differs from the runtime/SDK candidate")
            if metadata.get("User") != "65532:65532":
                raise ValueError("OCI image does not use the release non-root identity")
            platforms.append(selected)

        index = archive.getmember("index.json")
        if not index.isfile():
            raise ValueError("invalid OCI index")
        roots = json.load(archive.extractfile(index))["manifests"]
        if len(roots) != 1 or roots[0].get("mediaType") != "application/vnd.oci.image.index.v1+json":
            raise ValueError("candidate requires the single Buildx multi-platform index")
        for descriptor in roots:
            visit(descriptor)
    if set(platforms) != {"linux/amd64", "linux/arm64"}:
        raise ValueError("OCI candidate must contain both supported Linux platforms")
    return {"platforms": sorted(platforms), "digest": roots[0]["digest"]}


def finalize(folder):
    manifest = read_candidate(folder)
    if any(record["kind"] == "oci-image" for record in manifest["artifacts"]):
        raise ValueError("OCI image records already exist; candidate manifests are immutable")
    records = list(manifest["artifacts"])
    for image in ("mango", "mango-self-hosted-worker"):
        name = f"{image}-oci.tar"
        path = folder / name
        if path.is_symlink():
            raise ValueError("OCI candidate archives must not be symbolic links")
        identity = inspect_oci(path, manifest["version"], manifest["revision"])
        records.append({"name": name, "kind": "oci-image", "image": image, **identity})
    write_manifest(folder, manifest["version"], manifest["revision"], manifest["sdk_versions"], records)
    print("PASS: both multi-platform OCI archives share the command/SDK identity; manifest/checksums finalized")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("candidate", type=Path)
    arguments = parser.parse_args()
    finalize(arguments.candidate.resolve())
