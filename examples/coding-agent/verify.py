"""Independent acceptance checks in a disposable, restricted local Docker container."""

from pathlib import Path
import shutil
import subprocess
import tempfile
import uuid


def check_docker(image: str) -> None:
    try:
        subprocess.run(["docker", "image", "inspect", image], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
    except (OSError, subprocess.SubprocessError) as error:
        raise RuntimeError(f"Start Docker and pull the verification image first: docker pull {image}") from error


def verify_output(source: Path, image: str) -> None:
    name = "mango-example-verify-" + uuid.uuid4().hex
    with tempfile.TemporaryDirectory(prefix="mango-checks-") as temporary:
        checks = Path(temporary)
        shutil.copyfile(source, checks / "calc.py")
        shutil.copyfile(Path(__file__).parent / "fixtures/test_calc.py", checks / "test_calc.py")
        checks.chmod(0o755)
        for file in checks.iterdir():
            file.chmod(0o444)
        # region verify
        command = [
            "docker", "run", "--rm", "--pull=never", "--name", name,
            "--network=none", "--read-only", "--cap-drop=ALL",
            "--security-opt=no-new-privileges", "--pids-limit=64",
            "--memory=128m", "--cpus=1", "--user=65534:65534",
            "--mount", f"type=bind,src={checks.resolve()},dst=/checks,readonly",
            "--workdir=/checks", image, "python3", "-B", "test_calc.py",
        ]
        # The original tests are read-only; no Mango/model key or project directory
        # is mounted. Docker is a development isolation boundary, not a hostile-code SLA.
        with tempfile.TemporaryFile() as log:
            try:
                result = subprocess.run(command, stdout=log, stderr=log, timeout=20)
                if result.returncode != 0:
                    log.seek(0)
                    raise RuntimeError("Independent checks failed:\n" + log.read(16384).decode(errors="replace"))
            finally:
                # Kill a timed-out container, not just the waiting Docker client.
                subprocess.run(["docker", "rm", "--force", name],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
        # endregion verify
