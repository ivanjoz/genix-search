#!/usr/bin/env python3
"""Install GenixSearch as a systemd service.

By default nothing is compiled: the script detects the machine, downloads
the matching prebuilt binary from a GitHub release, verifies it against the
release's SHA256SUMS, then installs it plus its config under `/etc`, creates
a system user and state directory, writes a hardened unit file, and enables
and starts the service. The published binaries are fully static musl builds,
so there is no toolchain and no glibc requirement on the target host.

This file is self-sufficient — it fetches its own binary and reference
config — so it can be run straight off the internet:

    curl -fsSL https://raw.githubusercontent.com/ivanjoz/genix-search/main/scripts/install_service.py \
        | sudo python3 - install --bind 0.0.0.0:1491 --auth-password 'hunter2'

Usage:
    sudo python3 scripts/install_service.py                       # download + install + start
    sudo python3 scripts/install_service.py --dry-run             # print every action and file
    sudo python3 scripts/install_service.py --bind 0.0.0.0:1491 --auth-password 'hunter2'
    sudo python3 scripts/install_service.py --release-version v0.1.0
    sudo python3 scripts/install_service.py --flavor ampere       # force the Neoverse N1 build
    sudo python3 scripts/install_service.py --binary ./genixsearch  # install a local binary
    sudo python3 scripts/install_service.py --from-source         # compile it here instead
    sudo python3 scripts/install_service.py status
    sudo python3 scripts/install_service.py uninstall             # keeps config + data
    sudo python3 scripts/install_service.py uninstall --purge     # removes config, data, user

Layout installed (defaults):
    /usr/local/bin/genixsearch                  binary
    /etc/genixsearch/genixsearch.cfg            config (0640 root:genixsearch)
    /etc/genixsearch/genixsearch.env            optional GENIXSEARCH_* overrides
    /var/lib/genixsearch/                        RocksDB state (store/kv)
    /etc/systemd/system/genixsearch.service     unit
"""

import argparse
import atexit
import grp
import hashlib
import json
import os
import pathlib
import pwd
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request

# Piping this file into `python3 -` leaves __file__ undefined, which is a
# supported way to run it, so fall back instead of failing at import time.
try:
    REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
except NameError:  # pragma: no cover - only when read from stdin
    REPO_ROOT = pathlib.Path.cwd()

# In a checkout the Rust crate (Cargo.toml, genixsearch.cfg, target/) lives in
# server/; an unpacked release tarball has the config at its root instead.
SERVER_DIR = REPO_ROOT / "server" if (REPO_ROOT / "server" / "Cargo.toml").is_file() else REPO_ROOT

# Only present when running from a checkout or an unpacked release tarball;
# otherwise the reference config comes from the downloaded release.
SOURCE_CONFIG = SERVER_DIR / "genixsearch.cfg"

DEFAULT_NAME = "genixsearch"
DEFAULT_PREFIX = pathlib.Path("/usr/local")
DEFAULT_LOG_LEVEL = "error"

DEFAULT_REPO = "ivanjoz/genix-search"

NOLOGIN_CANDIDATES = ("/usr/sbin/nologin", "/sbin/nologin", "/usr/bin/false", "/bin/false")

# Native build dependencies: `librocksdb-sys` compiles RocksDB from source
# (C/C++ toolchain) and generates its bindings with bindgen (libclang), so a
# bare rustup install is not enough — the linker `cc` alone is what most
# minimal server images are missing. Package sets are per-manager, and cover
# x86_64 and aarch64 alike; nothing here is architecture-specific.
PACKAGE_MANAGERS: tuple[dict, ...] = (
    {
        "binary": "dnf",
        "distros": ("fedora", "rhel", "centos", "rocky", "almalinux", "amzn"),
        "refresh": None,
        "install": ["dnf", "install", "-y"],
        "packages": {
            "cc": ["gcc"],
            "c++": ["gcc-c++"],
            "make": ["make"],
            "libclang": ["clang", "clang-devel"],
        },
    },
    {
        "binary": "apt-get",
        "distros": ("debian", "ubuntu", "raspbian", "linuxmint", "pop"),
        "refresh": ["apt-get", "update"],
        "install": ["apt-get", "install", "-y", "--no-install-recommends"],
        "packages": {
            "cc": ["build-essential"],
            "c++": ["build-essential"],
            "make": ["build-essential"],
            "libclang": ["clang", "libclang-dev"],
        },
    },
    {
        "binary": "zypper",
        "distros": ("opensuse", "opensuse-leap", "opensuse-tumbleweed", "sles", "suse"),
        "refresh": None,
        "install": ["zypper", "--non-interactive", "install"],
        "packages": {
            "cc": ["gcc"],
            "c++": ["gcc-c++"],
            "make": ["make"],
            "libclang": ["clang", "clang-devel"],
        },
    },
    {
        "binary": "pacman",
        "distros": ("arch", "archlinux", "manjaro", "endeavouros"),
        "refresh": None,
        # `base-devel` is a group and prompts even with --noconfirm, so name
        # the individual packages instead.
        "install": ["pacman", "-S", "--needed", "--noconfirm"],
        "packages": {"cc": ["gcc"], "c++": ["gcc"], "make": ["make"], "libclang": ["clang"]},
    },
    {
        "binary": "apk",
        "distros": ("alpine",),
        "refresh": None,
        "install": ["apk", "add", "--no-cache"],
        "packages": {
            "cc": ["build-base"],
            "c++": ["build-base"],
            "make": ["build-base"],
            "libclang": ["clang", "clang-dev"],
        },
    },
    {
        "binary": "yum",
        "distros": ("rhel", "centos"),
        "refresh": None,
        "install": ["yum", "install", "-y"],
        "packages": {
            "cc": ["gcc"],
            "c++": ["gcc-c++"],
            "make": ["make"],
            "libclang": ["clang", "clang-devel"],
        },
    },
)

OS_RELEASE = pathlib.Path("/etc/os-release")

# Directories where clang-sys looks for libclang, across distro layouts.
# Names are versioned on some distros (Debian's `libclang-17.so.17`), hence
# the wide file glob below.
LIBCLANG_DIR_GLOBS = (
    "/usr/lib64",
    "/usr/lib",
    "/usr/lib/*-linux-gnu",
    "/usr/lib/llvm-*/lib",
    "/usr/lib64/llvm*/lib",
    "/usr/local/lib",
)

ENV_FILE_TEMPLATE = """\
# Environment overrides for GenixSearch, read by systemd (EnvironmentFile).
#
# Any GENIXSEARCH_* variable overrides the matching key of {config_path};
# the TOML path separator is `__`. Keep secrets here rather than in the
# config file if you prefer: this file is only readable by root and the
# service group.
#
#GENIXSEARCH_CHANNEL__AUTH_PASSWORD=change-me
#GENIXSEARCH_SERVER__LOG_LEVEL=info
"""


# -- shell plumbing ----------------------------------------------------------


class Fail(Exception):
    """Fatal, expected error: reported without a traceback."""


def log(message: str) -> None:
    print(f"\033[1;34m==>\033[0m {message}")


def warn(message: str) -> None:
    sys.stdout.flush()  # keep the interleaving readable when stdout is a pipe
    print(f"\033[1;33mwarning:\033[0m {message}", file=sys.stderr)


def run(
    cmd: list[str],
    *,
    dry_run: bool = False,
    check: bool = True,
    capture: bool = False,
    user: pwd.struct_passwd | None = None,
    cwd: pathlib.Path | None = None,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess:
    """Run `cmd`, optionally demoted to `user`. Honours `dry_run`."""
    shown = " ".join(cmd)
    if user is not None:
        shown = f"(as {user.pw_name}) {shown}"
    print(f"    $ {shown}")

    if dry_run:
        return subprocess.CompletedProcess(cmd, 0, "", "")

    preexec = None
    if user is not None and os.geteuid() == 0:

        def preexec() -> None:  # pragma: no cover — runs in the forked child
            os.setgid(user.pw_gid)
            os.initgroups(user.pw_name, user.pw_gid)
            os.setuid(user.pw_uid)

    result = subprocess.run(
        cmd,
        cwd=str(cwd) if cwd else None,
        env=env,
        preexec_fn=preexec,
        text=True,
        capture_output=capture,
    )
    if check and result.returncode != 0:
        if capture:
            sys.stderr.write(result.stdout or "")
            sys.stderr.write(result.stderr or "")
        raise Fail(f"command failed ({result.returncode}): {shown}")
    return result


def systemctl(*args: str, dry_run: bool = False, check: bool = True) -> subprocess.CompletedProcess:
    return run(["systemctl", *args], dry_run=dry_run, check=check)


def write_file(
    path: pathlib.Path,
    content: str,
    *,
    mode: int,
    owner: str = "root",
    group: str = "root",
    dry_run: bool = False,
) -> None:
    """Write `content` atomically with the given mode and ownership."""
    log(f"writing {path} ({oct(mode)[2:]} {owner}:{group})")
    if dry_run:
        print("    ┌─────")
        for line in content.splitlines():
            print(f"    │ {line}")
        print("    └─────")
        return

    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.tmp")
    tmp.write_text(content, encoding="utf-8")
    os.chmod(tmp, mode)
    shutil.chown(tmp, user=owner, group=group)
    tmp.replace(path)


def restore_selinux_context(paths: list[pathlib.Path], *, dry_run: bool = False) -> None:
    """Relabel installed files when SELinux is enforcing (Fedora, RHEL, ...)."""
    if not shutil.which("selinuxenabled") or not shutil.which("restorecon"):
        return
    # Read-only probe: worth running even for --dry-run.
    if run(["selinuxenabled"], check=False, capture=True).returncode != 0:
        return

    log("restoring SELinux contexts")
    run(
        ["restorecon", "-R", "-F", *[str(p) for p in paths]],
        dry_run=dry_run,
        check=False,
        capture=True,
    )


# -- preflight ---------------------------------------------------------------


def require_root() -> None:
    if os.geteuid() != 0:
        raise Fail("must run as root — re-run with `sudo`")


def require_systemd() -> None:
    if sys.platform != "linux":
        raise Fail(f"systemd services are Linux-only (running on {sys.platform})")
    if not pathlib.Path("/run/systemd/system").is_dir():
        raise Fail("systemd is not the active init system on this host")
    if not shutil.which("systemctl"):
        raise Fail("`systemctl` was not found in PATH")


def build_user() -> pwd.struct_passwd | None:
    """The unprivileged user cargo should build as, if we were sudo'd."""
    name = os.environ.get("SUDO_USER")
    if not name or name == "root":
        return None
    try:
        return pwd.getpwnam(name)
    except KeyError:
        return None


def resolve_cargo(user: pwd.struct_passwd | None) -> str:
    """Find cargo, preferring the build user's rustup installation."""
    candidates = []
    if user is not None:
        candidates.append(pathlib.Path(user.pw_dir) / ".cargo" / "bin" / "cargo")
    found = shutil.which("cargo")
    if found:
        candidates.append(pathlib.Path(found))

    for candidate in candidates:
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return str(candidate)
    raise Fail(
        "`cargo` was not found. Install Rust (https://rustup.rs) or pass "
        "--no-build --binary <path> to install a prebuilt binary."
    )


def have_libclang() -> bool:
    """Whether bindgen (librocksdb-sys) will find a libclang to load."""
    if os.environ.get("LIBCLANG_PATH"):
        return True

    root = pathlib.Path("/")
    for dir_glob in LIBCLANG_DIR_GLOBS:
        for directory in root.glob(dir_glob.lstrip("/")):
            for library in directory.glob("libclang*.so*"):
                # `libclang-cpp` is the C++ API; bindgen cannot use it.
                if not library.name.startswith("libclang-cpp"):
                    return True
    return False


def missing_build_capabilities() -> list[str]:
    """Native build deps that are absent. RocksDB is compiled from source."""
    missing = [tool for tool in ("cc", "c++", "make") if not shutil.which(tool)]
    if not have_libclang():
        missing.append("libclang")
    return missing


def detect_package_manager() -> dict | None:
    """Pick a package manager, preferring the one matching /etc/os-release."""
    distros: set[str] = set()
    try:
        for line in OS_RELEASE.read_text(encoding="utf-8").splitlines():
            key, _, value = line.partition("=")
            if key in ("ID", "ID_LIKE"):
                distros.update(value.strip().strip('"').split())
    except OSError:
        pass

    available = [manager for manager in PACKAGE_MANAGERS if shutil.which(manager["binary"])]
    for manager in available:
        if distros & set(manager["distros"]):
            return manager
    return available[0] if available else None


def install_build_deps(missing: list[str], *, dry_run: bool) -> None:
    """Install the native toolchain for `missing` using the host's packager."""
    manager = detect_package_manager()
    if manager is None:
        raise Fail(
            f"missing build dependencies ({', '.join(missing)}) and no supported package "
            f"manager was found ({', '.join(m['binary'] for m in PACKAGE_MANAGERS)}). "
            "Install a C/C++ toolchain plus libclang by hand, or pass "
            "--no-build --binary <path>."
        )

    packages: list[str] = []
    for capability in missing:
        for package in manager["packages"].get(capability, []):
            if package not in packages:
                packages.append(package)

    log(
        f"missing build dependencies ({', '.join(missing)}); installing "
        f"{', '.join(packages)} with {manager['binary']}"
    )

    env = dict(os.environ, DEBIAN_FRONTEND="noninteractive")
    if manager["refresh"]:
        run(manager["refresh"], dry_run=dry_run, env=env)
    run([*manager["install"], *packages], dry_run=dry_run, env=env)


def ensure_build_toolchain(args: argparse.Namespace) -> None:
    """Make sure the host can actually compile before invoking cargo."""
    missing = missing_build_capabilities()
    if not missing:
        return

    if not args.install_build_deps:
        raise Fail(
            f"missing build dependencies: {', '.join(missing)}. RocksDB is built from "
            "source and needs a C/C++ toolchain plus libclang. Drop "
            "--no-install-build-deps to let this script install them, or install them "
            "manually (Fedora: `dnf install gcc gcc-c++ make clang clang-devel`, "
            "Debian/Ubuntu: `apt install build-essential clang libclang-dev`)."
        )

    if os.geteuid() != 0 and not args.dry_run:
        raise Fail(f"root is required to install the missing build dependencies: {', '.join(missing)}")

    install_build_deps(missing, dry_run=args.dry_run)

    if args.dry_run:
        return

    still_missing = missing_build_capabilities()
    if still_missing:
        raise Fail(
            f"build dependencies still missing after installation: {', '.join(still_missing)}. "
            "Install them manually, then re-run with --no-install-build-deps."
        )
    log("build dependencies are in place")


# -- prebuilt releases -------------------------------------------------------

# Asset suffixes published by scripts/release_binaries.sh. Keep in step with
# scripts/util/release_targets.sh.
ASSET_X86_64 = "x86_64-linux-musl"
ASSET_AARCH64 = "aarch64-linux-musl"
ASSET_AARCH64_NEOVERSE_N1 = "aarch64-linux-musl-neoverse-n1"

_TEMP_DIRS: list[str] = []


def _cleanup_temp_dirs() -> None:
    for path in _TEMP_DIRS:
        shutil.rmtree(path, ignore_errors=True)


atexit.register(_cleanup_temp_dirs)


def is_neoverse_n1() -> bool:
    """True when this CPU is an Arm Neoverse N1 (Ampere Altra / Altra Max).

    MIDR_EL1 part 0xd0c is Neoverse N1, which is the core Oracle Cloud rents
    out as the "Ampere A1" (VM.Standard.A1.Flex) shape.
    """
    try:
        cpuinfo = pathlib.Path("/proc/cpuinfo").read_text(encoding="utf-8", errors="replace")
    except OSError:
        return False
    return any(
        line.split(":", 1)[1].strip().lower() == "0xd0c"
        for line in cpuinfo.splitlines()
        if line.lower().startswith("cpu part") and ":" in line
    )


def detect_asset_suffix(flavor: str) -> str:
    """Pick the release asset for this machine."""
    machine = os.uname().machine
    if machine in ("x86_64", "amd64"):
        if flavor != "auto":
            warn(f"--flavor {flavor} only applies to aarch64; ignoring it on {machine}")
        return ASSET_X86_64
    if machine in ("aarch64", "arm64"):
        if flavor == "ampere" or (flavor == "auto" and is_neoverse_n1()):
            return ASSET_AARCH64_NEOVERSE_N1
        return ASSET_AARCH64
    raise Fail(
        f"no prebuilt binary is published for {machine}; "
        "build from source instead with --from-source"
    )


def http_get(url: str, timeout: float = 60.0) -> bytes:
    # GitHub rejects requests without a User-Agent.
    request = urllib.request.Request(url, headers={"User-Agent": f"{DEFAULT_NAME}-install-service"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.read()
    except urllib.error.HTTPError as error:
        raise Fail(f"HTTP {error.code} fetching {url}") from error
    except urllib.error.URLError as error:
        raise Fail(f"could not fetch {url}: {error.reason}") from error


def resolve_latest_release(repo: str) -> str:
    log(f"resolving the latest release of {repo}")
    payload = http_get(f"https://api.github.com/repos/{repo}/releases/latest")
    try:
        tag = json.loads(payload)["tag_name"]
    except (ValueError, KeyError) as error:
        raise Fail(f"could not read the latest release tag for {repo}") from error
    if not tag:
        raise Fail(f"{repo} has no published releases")
    return tag


def _extract_tar(archive: pathlib.Path, dest: pathlib.Path) -> None:
    with tarfile.open(archive, "r:gz") as tar:
        try:
            # Python 3.12+ rejects unsafe members; older versions lack the
            # parameter entirely, hence the fallback.
            tar.extractall(dest, filter="data")
        except TypeError:
            tar.extractall(dest)


def download_release(args: argparse.Namespace) -> tuple[pathlib.Path, pathlib.Path]:
    """Fetch the release asset for this machine.

    Returns `(binary, reference_config)` inside a temporary directory that is
    removed when the process exits.
    """
    suffix = detect_asset_suffix(args.flavor)
    version = args.release_version
    if version in (None, "latest"):
        version = resolve_latest_release(args.repo)
    if not version.startswith("v"):
        version = f"v{version}"

    stem = f"{DEFAULT_NAME}-{version}-{suffix}"
    archive_name = f"{stem}.tar.gz"
    base = f"https://github.com/{args.repo}/releases/download/{version}"

    log(f"selected {archive_name} for {os.uname().machine}")

    if args.dry_run:
        log(f"would download {base}/{archive_name}")
        log("would verify it against the release SHA256SUMS")
        # Nothing is on disk in a dry run, so hand back the paths the real run
        # would use; no caller reads them when dry_run is set.
        placeholder = pathlib.Path(tempfile.gettempdir()) / stem
        return placeholder / DEFAULT_NAME, placeholder / f"{DEFAULT_NAME}.cfg"

    workdir = pathlib.Path(tempfile.mkdtemp(prefix=f"{DEFAULT_NAME}-release-"))
    _TEMP_DIRS.append(str(workdir))

    log(f"downloading {base}/{archive_name}")
    payload = http_get(f"{base}/{archive_name}")
    archive = workdir / archive_name
    archive.write_bytes(payload)

    # Verify against the release manifest. A corrupted or substituted asset is
    # otherwise only noticed when the service fails to start.
    digest = hashlib.sha256(payload).hexdigest()
    try:
        manifest = http_get(f"{base}/SHA256SUMS").decode("utf-8", errors="replace")
    except Fail:
        warn(f"{version} publishes no SHA256SUMS; cannot verify {archive_name}")
    else:
        expected = None
        for line in manifest.splitlines():
            parts = line.split()
            if len(parts) == 2 and parts[1] == archive_name:
                expected = parts[0]
                break
        if expected is None:
            warn(f"SHA256SUMS has no entry for {archive_name}; skipping verification")
        elif expected != digest:
            raise Fail(
                f"checksum mismatch for {archive_name}: "
                f"expected {expected}, got {digest}"
            )
        else:
            log("checksum verified against the release SHA256SUMS")

    _extract_tar(archive, workdir)

    binary = workdir / stem / DEFAULT_NAME
    if not binary.is_file():
        raise Fail(f"{archive_name} did not contain {stem}/{DEFAULT_NAME}")
    binary.chmod(0o755)

    config = workdir / stem / f"{DEFAULT_NAME}.cfg"
    return binary, config


def resolve_source_config(downloaded: pathlib.Path | None) -> pathlib.Path:
    """Find the reference config to base the installed one on."""
    candidates = []
    if downloaded is not None:
        candidates.append(downloaded)
    candidates.append(SOURCE_CONFIG)

    for candidate in candidates:
        if candidate.is_file():
            return candidate

    raise Fail(
        "cannot find a reference genixsearch.cfg. Looked in: "
        + ", ".join(str(candidate) for candidate in candidates)
    )


# -- build -------------------------------------------------------------------


def cargo_build(args: argparse.Namespace) -> pathlib.Path:
    """Build the release binary and return its path."""
    user = build_user()
    if user is None and os.geteuid() == 0:
        warn(
            "no SUDO_USER to drop privileges to — cargo will run as root and "
            "target/ will become root-owned"
        )

    cargo = resolve_cargo(user)
    ensure_build_toolchain(args)

    cmd = [cargo, "build", "--locked", "--bin", DEFAULT_NAME]
    if args.profile == "release":
        cmd.append("--release")
    if args.target:
        cmd += ["--target", args.target]

    env = dict(os.environ)
    env.pop("SUDO_USER", None)
    env.pop("SUDO_UID", None)
    env.pop("SUDO_GID", None)
    if user is not None:
        home = user.pw_dir
        env["HOME"] = home
        env["USER"] = user.pw_name
        env["LOGNAME"] = user.pw_name
        env["PATH"] = f"{home}/.cargo/bin:{env.get('PATH', '/usr/bin:/bin')}"

    log(f"building genixsearch ({args.profile}{f', {args.target}' if args.target else ''})")
    run(cmd, dry_run=args.dry_run, user=user, cwd=SERVER_DIR, env=env)

    out = SERVER_DIR / "target"
    if args.target:
        out = out / args.target
    binary = out / args.profile / DEFAULT_NAME

    if not binary.is_file() and not args.dry_run:
        raise Fail(f"build finished but no binary at {binary}")
    return binary


# -- config ------------------------------------------------------------------

_SECTION_RE = re.compile(r"\s*\[([^\]]+)\]")
_KEY_RE = re.compile(r"(\s*)([A-Za-z0-9_.\-]+)(\s*=\s*)(.*)$")


def rewrite_config(source: str, overrides: dict[tuple[str, str], str]) -> str:
    """Apply `(section, key) -> TOML value` to `source`.

    Comments, key order and untouched values are preserved. A key that is
    absent is inserted at the end of its section, or in a new section.
    """
    lines = source.splitlines()
    out = list(lines)
    section_end: dict[str, int] = {"": 0}
    section = ""
    applied: set[tuple[str, str]] = set()

    for index, line in enumerate(lines):
        match_section = _SECTION_RE.match(line)
        if match_section:
            section = match_section.group(1).strip()
            section_end[section] = index + 1
            continue

        match_key = _KEY_RE.match(line)
        if not match_key:
            continue
        section_end[section] = index + 1

        indent, key, sep, _ = match_key.groups()
        target = (section, key)
        if target in overrides:
            out[index] = f"{indent}{key}{sep}{overrides[target]}"
            applied.add(target)

    # Insert the keys the file did not already carry, bottom-up so the
    # recorded indices stay valid.
    missing = [(sec, key) for (sec, key) in overrides if (sec, key) not in applied]
    inserts: list[tuple[int, str]] = []
    new_sections: list[str] = []
    for sec, key in missing:
        line = f"{key} = {overrides[(sec, key)]}"
        if sec in section_end:
            inserts.append((section_end[sec], line))
        else:
            new_sections.append(f"\n[{sec}]\n{line}")

    for index, line in sorted(inserts, key=lambda item: item[0], reverse=True):
        out.insert(index, line)

    return "\n".join(out).rstrip("\n") + "\n" + "".join(new_sections)


def render_config(
    args: argparse.Namespace, data_dir: pathlib.Path, source_config: pathlib.Path
) -> str:
    overrides: dict[tuple[str, str], str] = {
        ("server", "log_level"): json.dumps(args.log_level),
        ("channel", "inet"): json.dumps(args.bind),
        ("store.kv", "path"): json.dumps(f"{data_dir}/store/kv/"),
    }
    if args.auth_password is not None:
        overrides[("channel", "auth_password")] = json.dumps(args.auth_password)

    header = (
        f"# Installed by scripts/install_service.py for the {args.name}.service unit.\n"
        f"# Overrides can also be set as GENIXSEARCH_* variables in the unit's\n"
        f"# EnvironmentFile; see CONFIGURATION.md for the full reference.\n\n"
    )
    return header + rewrite_config(source_config.read_text(encoding="utf-8"), overrides)


# -- unit --------------------------------------------------------------------


def bind_port(bind: str) -> int:
    host, _, port = bind.rpartition(":")
    if not host or not port.isdigit():
        raise Fail(f"--bind must be <address>:<port> (got {bind!r})")
    return int(port)


def render_unit(
    args: argparse.Namespace,
    binary: pathlib.Path,
    config_path: pathlib.Path,
    env_path: pathlib.Path,
    data_dir: pathlib.Path,
) -> str:
    privileged_port = bind_port(args.bind) < 1024
    capabilities = "CAP_NET_BIND_SERVICE" if privileged_port else ""

    return f"""\
# GenixSearch systemd unit — installed by scripts/install_service.py.
# Edit freely; re-running the installer overwrites this file.
[Unit]
Description=GenixSearch — compact, lossy, ranked search backend
Documentation=file://{config_path}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={args.user}
Group={args.group}
WorkingDirectory={data_dir}
EnvironmentFile=-{env_path}
ExecStart={binary} --config {config_path}
Restart=on-failure
RestartSec=2s
# The KV store is flushed synchronously on SIGTERM; give it room.
KillSignal=SIGTERM
TimeoutStopSec=120s
LimitNOFILE=65536
SyslogIdentifier={args.name}

# Hardening. Relax these if you extend GenixSearch beyond listening on a
# socket and writing to its state directory.
AmbientCapabilities={capabilities}
CapabilityBoundingSet={capabilities}
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectProc=invisible
ProtectClock=true
ProtectHostname=true
ProtectKernelLogs=true
ProtectKernelModules=true
ProtectKernelTunables=true
ProtectControlGroups=true
ReadWritePaths={data_dir}
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service

[Install]
WantedBy=multi-user.target
"""


# -- user / directories ------------------------------------------------------


def ensure_group(name: str, *, dry_run: bool) -> None:
    try:
        grp.getgrnam(name)
        log(f"group {name} already exists")
    except KeyError:
        log(f"creating system group {name}")
        run(["groupadd", "--system", name], dry_run=dry_run)


def ensure_user(name: str, group: str, home: pathlib.Path, *, dry_run: bool) -> None:
    try:
        pwd.getpwnam(name)
        log(f"user {name} already exists")
        return
    except KeyError:
        pass

    shell = next((path for path in NOLOGIN_CANDIDATES if pathlib.Path(path).exists()), "/bin/false")
    log(f"creating system user {name}")
    run(
        [
            "useradd",
            "--system",
            "--gid", group,
            "--home-dir", str(home),
            "--no-create-home",
            "--shell", shell,
            "--comment", "GenixSearch search backend",
            name,
        ],
        dry_run=dry_run,
    )


def ensure_data_dir(path: pathlib.Path, owner: str, group: str, *, dry_run: bool) -> None:
    log(f"preparing state directory {path} (0750 {owner}:{group})")
    if dry_run:
        return
    (path / "store" / "kv").mkdir(parents=True, exist_ok=True)
    for target in (path, path / "store", path / "store" / "kv"):
        os.chmod(target, 0o750)
        shutil.chown(target, user=owner, group=group)


# -- commands ----------------------------------------------------------------


def paths_for(args: argparse.Namespace) -> dict[str, pathlib.Path]:
    prefix = pathlib.Path(args.prefix)
    return {
        "binary": prefix / "bin" / args.name,
        "config_dir": pathlib.Path(args.config_dir),
        "config": pathlib.Path(args.config_dir) / f"{DEFAULT_NAME}.cfg",
        "env": pathlib.Path(args.config_dir) / f"{DEFAULT_NAME}.env",
        "data": pathlib.Path(args.data_dir),
        "unit": pathlib.Path("/etc/systemd/system") / f"{args.name}.service",
    }


def cmd_install(args: argparse.Namespace) -> int:
    require_systemd()
    if not args.dry_run:
        require_root()
    paths = paths_for(args)
    bind_port(args.bind)  # fail before building rather than half-way through

    downloaded_config: pathlib.Path | None = None
    if args.binary:
        source_binary = pathlib.Path(args.binary).resolve()
        if not source_binary.is_file():
            raise Fail(f"--binary does not exist: {source_binary}")
    elif args.from_source:
        source_binary = cargo_build(args)
    else:
        # The default: no compiler on the target host, no glibc requirement.
        source_binary, downloaded_config = download_release(args)

    ensure_group(args.group, dry_run=args.dry_run)
    ensure_user(args.user, args.group, paths["data"], dry_run=args.dry_run)
    ensure_data_dir(paths["data"], args.user, args.group, dry_run=args.dry_run)

    log(f"installing {source_binary} -> {paths['binary']}")
    if not args.dry_run:
        paths["binary"].parent.mkdir(parents=True, exist_ok=True)
    # install(1) over cp: it replaces the inode, so a running service keeps
    # executing its current binary until restarted.
    run(
        ["install", "-m", "0755", "-o", "root", "-g", "root", str(source_binary), str(paths["binary"])],
        dry_run=args.dry_run,
    )

    try:
        source_config = resolve_source_config(downloaded_config)
    except Fail:
        # A dry run downloads nothing, so standalone there is no reference
        # config to preview yet. That is not a reason to refuse the rehearsal.
        if not args.dry_run:
            raise
        source_config = None

    if paths["config"].exists() and not args.force:
        log(f"keeping existing config {paths['config']} (pass --force to overwrite)")
    elif source_config is None:
        log(f"would write {paths['config']} from the config shipped in the release archive")
    else:
        config = render_config(args, paths["data"], source_config)
        if paths["config"].exists():
            backup = paths["config"].with_suffix(".cfg.bak")
            log(f"backing up existing config to {backup}")
            if not args.dry_run:
                shutil.copy2(paths["config"], backup)
        write_file(paths["config"], config, mode=0o640, group=args.group, dry_run=args.dry_run)

    if not paths["env"].exists():
        write_file(
            paths["env"],
            ENV_FILE_TEMPLATE.format(config_path=paths["config"]),
            mode=0o640,
            group=args.group,
            dry_run=args.dry_run,
        )

    unit = render_unit(args, paths["binary"], paths["config"], paths["env"], paths["data"])
    write_file(paths["unit"], unit, mode=0o644, dry_run=args.dry_run)

    restore_selinux_context(
        [paths["binary"], paths["config_dir"], paths["data"]], dry_run=args.dry_run
    )

    if not args.dry_run:
        version = run([str(paths["binary"]), "--version"], capture=True, check=False)
        log(f"installed binary reports: {(version.stdout or version.stderr).strip() or 'no version output'}")

    log("reloading systemd")
    systemctl("daemon-reload", dry_run=args.dry_run)

    if args.enable:
        log(f"enabling {args.name}.service at boot")
        systemctl("enable", f"{args.name}.service", dry_run=args.dry_run)

    if args.start:
        log(f"starting {args.name}.service")
        systemctl("restart", f"{args.name}.service", dry_run=args.dry_run)
        if not args.dry_run and not wait_active(args.name):
            print_recent_logs(args.name)
            raise Fail(
                f"{args.name}.service did not stay active — see the log excerpt above "
                f"(`journalctl -u {args.name} -e`)"
            )

    print_summary(args, paths)
    return 0


def wait_active(name: str, timeout: float = 10.0) -> bool:
    """Poll `is-active` until the unit settles, so failures surface here."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        state = subprocess.run(
            ["systemctl", "is-active", f"{name}.service"],
            text=True,
            capture_output=True,
        ).stdout.strip()
        if state == "active":
            time.sleep(1.0)  # catch an immediate crash-on-startup
            state = subprocess.run(
                ["systemctl", "is-active", f"{name}.service"],
                text=True,
                capture_output=True,
            ).stdout.strip()
            return state == "active"
        if state in ("failed", "inactive"):
            return False
        time.sleep(0.4)
    return False


def print_recent_logs(name: str, lines: int = 30) -> None:
    subprocess.run(["journalctl", "-u", f"{name}.service", "-n", str(lines), "--no-pager"], check=False)


def print_summary(args: argparse.Namespace, paths: dict[str, pathlib.Path]) -> None:
    print()
    log("done" + (" (dry run — nothing was changed)" if args.dry_run else ""))
    print(f"    binary   {paths['binary']}")
    print(f"    config   {paths['config']}")
    print(f"    env      {paths['env']}")
    print(f"    state    {paths['data']}")
    print(f"    unit     {paths['unit']}")
    print(f"    listen   {args.bind}")
    print()
    print(f"    systemctl status {args.name}")
    print(f"    journalctl -u {args.name} -f")
    print(f"    python3 scripts/search_cli.py --host {args.bind.rpartition(':')[0].strip('[]') or '::1'} "
          f"--port {bind_port(args.bind)}")


def cmd_uninstall(args: argparse.Namespace) -> int:
    require_systemd()
    if not args.dry_run:
        require_root()
    paths = paths_for(args)

    log(f"stopping and disabling {args.name}.service")
    systemctl("stop", f"{args.name}.service", dry_run=args.dry_run, check=False)
    systemctl("disable", f"{args.name}.service", dry_run=args.dry_run, check=False)

    for path in (paths["unit"], paths["binary"]):
        if path.exists():
            log(f"removing {path}")
            if not args.dry_run:
                path.unlink()
        else:
            log(f"not present: {path}")

    systemctl("daemon-reload", dry_run=args.dry_run)
    systemctl("reset-failed", f"{args.name}.service", dry_run=args.dry_run, check=False)

    if args.purge:
        for path in (paths["config_dir"], paths["data"]):
            if path.exists():
                log(f"purging {path}")
                if not args.dry_run:
                    shutil.rmtree(path)
        try:
            pwd.getpwnam(args.user)
            log(f"removing user {args.user}")
            run(["userdel", args.user], dry_run=args.dry_run, check=False)
        except KeyError:
            pass
        try:
            grp.getgrnam(args.group)
            log(f"removing group {args.group}")
            run(["groupdel", args.group], dry_run=args.dry_run, check=False)
        except KeyError:
            pass
    else:
        log(f"kept config {paths['config_dir']} and state {paths['data']} (pass --purge to remove)")

    print()
    log("uninstalled" + (" (dry run — nothing was changed)" if args.dry_run else ""))
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    require_systemd()
    subprocess.run(["systemctl", "status", f"{args.name}.service", "--no-pager"], check=False)
    print()
    print_recent_logs(args.name, lines=20)
    return 0


# -- cli ---------------------------------------------------------------------


def add_common(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--name", default=DEFAULT_NAME, help="service/unit name (default: %(default)s)")
    parser.add_argument("--user", default=DEFAULT_NAME, help="system user to run as (default: %(default)s)")
    parser.add_argument("--group", default=DEFAULT_NAME, help="system group (default: %(default)s)")
    parser.add_argument("--prefix", default=str(DEFAULT_PREFIX), help="binary install prefix (default: %(default)s)")
    parser.add_argument("--config-dir", default=f"/etc/{DEFAULT_NAME}", help="config directory (default: %(default)s)")
    parser.add_argument("--data-dir", default=f"/var/lib/{DEFAULT_NAME}", help="state directory (default: %(default)s)")
    parser.add_argument("--dry-run", action="store_true", help="print actions and rendered files, change nothing")


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="install_service.py",
        description=__doc__.splitlines()[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="Run without a subcommand to install.",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    install = subparsers.add_parser("install", help="download and install the service")
    add_common(install)
    install.add_argument("--bind", default="[::1]:1491", help="channel listen address (default: %(default)s)")
    install.add_argument(
        "--auth-password",
        default=None,
        help="channel auth password; omitted keeps the value from genixsearch.cfg",
    )
    install.add_argument(
        "--log-level",
        default=DEFAULT_LOG_LEVEL,
        choices=("debug", "info", "warn", "error"),
        help="server log level (default: %(default)s)",
    )
    install.add_argument(
        "--repo", default=DEFAULT_REPO, help="GitHub repository to download from (default: %(default)s)"
    )
    install.add_argument(
        "--release-version",
        default="latest",
        help="release tag to install, e.g. v0.1.0 (default: %(default)s)",
    )
    install.add_argument(
        "--flavor",
        default="auto",
        choices=("auto", "generic", "ampere"),
        help=(
            "which aarch64 build to take; 'auto' picks the Neoverse N1 build on "
            "an Ampere Altra (default: %(default)s)"
        ),
    )
    install.add_argument("--binary", default=None, help="install this local binary instead of downloading")
    install.add_argument(
        "--from-source",
        action="store_true",
        help="compile with cargo instead of downloading a prebuilt release",
    )
    install.add_argument(
        "--profile", default="release", choices=("release", "debug"), help="cargo profile (default: %(default)s)"
    )
    install.add_argument("--target", default=None, help="cargo --target triple (for cross-built hosts)")
    install.add_argument(
        "--no-build",
        action="store_true",
        help="accepted for compatibility; not compiling is now the default",
    )
    install.add_argument(
        "--no-install-build-deps",
        dest="install_build_deps",
        action="store_false",
        help="with --from-source, fail instead of installing a C/C++ toolchain and libclang",
    )
    install.add_argument("--force", action="store_true", help="overwrite an existing installed config")
    install.add_argument(
        "--no-enable", dest="enable", action="store_false", help="do not enable the unit at boot"
    )
    install.add_argument("--no-start", dest="start", action="store_false", help="do not start the unit now")
    install.set_defaults(func=cmd_install)

    uninstall = subparsers.add_parser("uninstall", help="stop and remove the service")
    add_common(uninstall)
    uninstall.add_argument("--purge", action="store_true", help="also remove config, state, user and group")
    uninstall.set_defaults(func=cmd_uninstall)

    status = subparsers.add_parser("status", help="show unit status and recent logs")
    status.add_argument("--name", default=DEFAULT_NAME, help="service/unit name (default: %(default)s)")
    status.set_defaults(func=cmd_status)

    # `install` is the default subcommand.
    if not argv or (argv[0].startswith("-") and argv[0] not in ("-h", "--help")):
        argv = ["install", *argv]
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    try:
        return args.func(args)
    except Fail as error:
        sys.stdout.flush()
        print(f"\033[1;31merror:\033[0m {error}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("\ninterrupted", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
