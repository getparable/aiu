"""Package the native executable and user documentation for a release target."""
import argparse
import os
from pathlib import Path
import tarfile
import tomllib
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check-tag", action="store_true")
    parser.add_argument("--target")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    version = tomllib.loads((root / "Cargo.toml").read_text())["package"]["version"]
    if args.check_tag:
        if os.environ.get("RELEASE_REF") != "tag" or os.environ.get("RELEASE_TAG") != f"v{version}":
            raise SystemExit(f"Run this workflow from the v{version} tag.")
        return
    if not args.target:
        parser.error("--target is required when packaging")
    supported = {
        "x86_64-pc-windows-msvc",
        "x86_64-unknown-linux-gnu",
        "aarch64-apple-darwin",
        "x86_64-apple-darwin",
    }
    if args.target not in supported:
        parser.error("unsupported release target")
    windows = "windows" in args.target
    executable = "aiu.exe" if windows else "aiu"
    binary = root / "target" / args.target / "release" / executable
    files = [(binary, executable)] + [(root / name, name) for name in ("README.md", "LICENSE")]
    for path, _ in files:
        if not path.is_file():
            raise SystemExit(f"Missing release file: {path}")
    destination = root / "dist"
    destination.mkdir(exist_ok=True)
    archive = destination / f"aiu-v{version}-{args.target}"
    if windows:
        archive = Path(f"{archive}.zip")
        with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as package:
            for path, name in files:
                package.write(path, name)
    else:
        archive = Path(f"{archive}.tar.gz")
        with tarfile.open(archive, "w:gz") as package:
            for path, name in files:
                package.add(path, arcname=name)
    print(archive)


if __name__ == "__main__":
    main()
