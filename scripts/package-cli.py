#!/usr/bin/env python3
"""Build standalone AIU CLI archives for the supported non-macOS targets."""

from __future__ import annotations

import argparse
import hashlib
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import tarfile
import zipfile


TARGETS = (("windows", "amd64"), ("windows", "arm64"), ("linux", "amd64"), ("linux", "arm64"))


def version_from_makefile(path: Path) -> str:
    match = re.search(r"^VERSION\s*\?=\s*([^\s#]+)", path.read_text(encoding="utf-8"), re.MULTILINE)
    if not match:
        raise SystemExit("Makefile does not define VERSION")
    return match.group(1)


def archive_name(version: str, goos: str, goarch: str) -> str:
    suffix = ".zip" if goos == "windows" else ".tar.gz"
    return f"aiu-{version}-{goos}-{goarch}{suffix}"


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def build_archive(root: Path, output: Path, version: str, goos: str, goarch: str) -> Path:
    output.mkdir(parents=True, exist_ok=True)
    name = archive_name(version, goos, goarch)
    with tempfile.TemporaryDirectory(prefix="aiu-package-") as temporary:
        staging = Path(temporary) / "aiu"
        staging.mkdir()
        binary = staging / ("aiu.exe" if goos == "windows" else "aiu")
        environment = os.environ.copy()
        environment.update({"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch})
        subprocess.run(
            [
                "go",
                "build",
                "-trimpath",
                "-ldflags",
                f"-s -w -X github.com/getparable/aiu/internal/core.Version={version}",
                "-o",
                str(binary),
                "./cmd/aiu",
            ],
            cwd=root,
            env=environment,
            check=True,
        )
        shutil.copyfile(root / "LICENSE", staging / "LICENSE")
        shutil.copyfile(root / "docs" / "platform-support.md", staging / "CLI.md")
        archive = output / name
        if goos == "windows":
            with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as package:
                for path in sorted(staging.iterdir()):
                    package.write(path, f"aiu/{path.name}")
        else:
            # Explicit archive modes also work when cross-building on Windows,
            # whose chmod bits cannot represent an executable Unix file.
            with tarfile.open(archive, "w:gz") as package:
                for path in sorted(staging.iterdir()):
                    info = package.gettarinfo(str(path), f"aiu/{path.name}")
                    info.mode = 0o755 if path == binary else 0o644
                    info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    with path.open("rb") as stream:
                        package.addfile(info, stream)
        return archive


def main() -> None:
    parser = argparse.ArgumentParser(description="Build standalone AIU CLI archives")
    parser.add_argument("--output", type=Path, default=Path("dist"))
    parser.add_argument("--version", help="release version (defaults to Makefile VERSION)")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    version = args.version or version_from_makefile(root / "Makefile")
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:[-+][A-Za-z0-9.-]+)?", version):
        parser.error("version must be a numeric release version, optionally with a suffix")
    archives = [build_archive(root, args.output, version, goos, goarch) for goos, goarch in TARGETS]
    checksums = args.output / f"aiu-{version}-checksums.txt"
    checksums.write_text("".join(f"{sha256(path)}  {path.name}\n" for path in archives), encoding="utf-8")
    for path in archives:
        print(path)
    print(checksums)


if __name__ == "__main__":
    main()
