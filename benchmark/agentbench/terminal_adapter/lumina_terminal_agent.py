import os
import shlex
from pathlib import Path

from terminal_bench.agents.base_agent import AgentResult
from terminal_bench.agents.installed_agents.abstract_installed_agent import (
    AbstractInstalledAgent,
)
from terminal_bench.terminal.models import TerminalCommand
from terminal_bench.terminal.tmux_session import TmuxSession


class LuminaTerminalAgent(AbstractInstalledAgent):
    @staticmethod
    def name() -> str:
        return "lumina"

    def __init__(self, install_script: str | None = None, **kwargs):
        super().__init__(**kwargs)
        self._install_script = install_script or os.environ.get(
            "LUMINA_TBENCH_INSTALL_SCRIPT", ""
        )

    @property
    def _env(self) -> dict[str, str]:
        env = {"LUMINA_RESOURCE_ROOT": "/usr/local/share/lumina"}
        keys = [
            "LUMINA_API_KEY",
            "LUMINA_API_BASE_URL",
            "LUMINA_API_MODEL",
            "LUMINA_API_TYPE",
            "ANTHROPIC_API_KEY",
            "ANTHROPIC_BASE_URL",
            "ANTHROPIC_MODEL",
            "LUMINA_MAX_PARENT_TURNS",
            "HTTPS_PROXY",
            "HTTP_PROXY",
            "NO_PROXY",
            "https_proxy",
            "http_proxy",
            "no_proxy",
        ]
        env.update({key: os.environ[key] for key in keys if os.environ.get(key)})
        return env

    @property
    def _install_agent_script_path(self) -> Path:
        if not self._install_script:
            raise ValueError(
                "LuminaTerminalAgent requires install_script=... or "
                "LUMINA_TBENCH_INSTALL_SCRIPT."
            )
        return Path(self._install_script)

    def _run_agent_commands(self, instruction: str) -> list[TerminalCommand]:
        escaped_instruction = shlex.quote(instruction)
        command = (
            "cat > /tmp/lumina-task.txt <<'LUMINA_TASK_EOF'\n"
            f"{instruction}\n"
            "LUMINA_TASK_EOF\n"
            "export LUMINA_HARNESS_MODE=terminal-bench\n"
            "mkdir -p /logs\n"
            "cat > /tmp/lumina-artifact-check.py <<'PY'\n"
            "import json\n"
            "import os\n"
            "import re\n"
            "import sys\n"
            "from pathlib import Path\n"
            "\n"
            "DIAG = Path('/logs/lumina-diagnostics.json')\n"
            "MISSING = Path('/tmp/lumina-missing-artifacts.txt')\n"
            "TASK = Path('/tmp/lumina-task.txt')\n"
            "PROCESS_SNAPSHOT = Path('/logs/lumina-processes.txt')\n"
            "PATH_RE = re.compile(r\"(?<![A-Za-z0-9_])(?:/(?:app|tmp|workspace|root)[^\\s`\\\"'<>),;]*|results/[^\\s`\\\"'<>),;]*)\")\n"
            "ARTIFACT_EXTS = ('.txt', '.json', '.jsonl', '.csv', '.tsv', '.xml', '.yaml', '.yml', '.html', '.md', '.pkl', '.pickle', '.png', '.jpg', '.jpeg')\n"
            "\n"
            "def load_diag():\n"
            "    try:\n"
            "        return json.loads(DIAG.read_text())\n"
            "    except Exception:\n"
            "        return {}\n"
            "\n"
            "def save_diag(diag):\n"
            "    DIAG.write_text(json.dumps(diag, ensure_ascii=False, indent=2) + '\\n')\n"
            "\n"
            "def clean_candidate(value):\n"
            "    return value.strip().strip('.,;:)\\\\]}>')\n"
            "\n"
            "def looks_like_artifact(path):\n"
            "    if path.startswith('/tests'):\n"
            "        return False\n"
            "    base = path.rsplit('/', 1)[-1]\n"
            "    if '.' in base and path.lower().endswith(ARTIFACT_EXTS):\n"
            "        return True\n"
            "    return path.startswith('results/')\n"
            "\n"
            "def extract_paths(text):\n"
            "    seen = set()\n"
            "    paths = []\n"
            "    for match in PATH_RE.finditer(text):\n"
            "        path = clean_candidate(match.group(0))\n"
            "        if not path or path in seen or not looks_like_artifact(path):\n"
            "            continue\n"
            "        seen.add(path)\n"
            "        paths.append(path)\n"
            "    return paths\n"
            "\n"
            "def check_paths(paths):\n"
            "    checks = []\n"
            "    missing = []\n"
            "    for path in paths:\n"
            "        concrete = '{' not in path and '}' not in path\n"
            "        exists = False\n"
            "        size = None\n"
            "        if concrete:\n"
            "            p = Path(path)\n"
            "            exists = p.exists()\n"
            "            if exists:\n"
            "                try:\n"
            "                    size = p.stat().st_size\n"
            "                except OSError:\n"
            "                    size = None\n"
            "            elif path.startswith('results/'):\n"
            "                p = Path('/app') / path\n"
            "                exists = p.exists()\n"
            "                if exists:\n"
            "                    try:\n"
            "                        size = p.stat().st_size\n"
            "                    except OSError:\n"
            "                        size = None\n"
            "        checks.append({'path': path, 'concrete': concrete, 'exists': exists, 'size_bytes': size})\n"
            "        if concrete and not exists:\n"
            "            missing.append(path)\n"
            "    return checks, missing\n"
            "\n"
            "def high_cpu_processes():\n"
            "    if not PROCESS_SNAPSHOT.exists():\n"
            "        return []\n"
            "    lines = PROCESS_SNAPSHOT.read_text(errors='replace').splitlines()\n"
            "    high = []\n"
            "    for line in lines[1:]:\n"
            "        parts = line.split(None, 4)\n"
            "        if len(parts) < 3:\n"
            "            continue\n"
            "        try:\n"
            "            cpu = float(parts[2])\n"
            "        except ValueError:\n"
            "            continue\n"
            "        if cpu >= 50:\n"
            "            high.append(line)\n"
            "    return high[:20]\n"
            "\n"
            "def classify(diag, missing):\n"
            "    if missing:\n"
            "        return 'missing_artifact'\n"
            "    if diag.get('agent_exit_status') not in (None, 0):\n"
            "        return 'agent_timeout' if diag.get('agent_timeout') else 'model_task_failure'\n"
            "    return ''\n"
            "\n"
            "phase = sys.argv[1] if len(sys.argv) > 1 else 'initial'\n"
            "status = int(sys.argv[2]) if len(sys.argv) > 2 and sys.argv[2].lstrip('-').isdigit() else None\n"
            "instruction = TASK.read_text(errors='replace') if TASK.exists() else ''\n"
            "paths = extract_paths(instruction)\n"
            "checks, missing = check_paths(paths)\n"
            "diag = load_diag()\n"
            "previous_missing = diag.get('explicit_missing_artifacts', [])\n"
            "diag['instruction_path'] = str(TASK)\n"
            "diag['explicit_artifact_checks'] = checks\n"
            "diag['explicit_missing_artifacts'] = missing\n"
            "diag['process_snapshot_path'] = str(PROCESS_SNAPSHOT)\n"
            "diag['high_cpu_processes'] = high_cpu_processes()\n"
            "diag.setdefault('post_flight_repair', {'triggered': False})\n"
            "if phase == 'initial':\n"
            "    diag['agent_exit_status'] = status\n"
            "    if missing:\n"
            "        MISSING.write_text('\\n'.join(missing) + '\\n')\n"
            "    else:\n"
            "        MISSING.write_text('')\n"
            "elif phase == 'repair':\n"
            "    diag['post_flight_repair'] = {'triggered': True, 'exit_status': status, 'missing_before_repair': previous_missing}\n"
            "elif phase == 'final':\n"
            "    diag['final_agent_exit_status'] = status\n"
            "diag['failure_category'] = classify(diag, missing)\n"
            "save_diag(diag)\n"
            "PY\n"
            "lumina_status=0\n"
            "lumina --bare --yolo --harness-mode terminal-bench -p \"$(cat /tmp/lumina-task.txt)\" || lumina_status=$?\n"
            "python3 /tmp/lumina-artifact-check.py initial \"${lumina_status}\" || true\n"
            "repair_status=0\n"
            "repair_ran=0\n"
            "if [ -s /tmp/lumina-missing-artifacts.txt ]; then\n"
            "  repair_ran=1\n"
            "  {\n"
            "    echo \"Terminal-Bench post-flight repair.\"\n"
            "    echo \"The previous attempt exited, but these explicit benchmark artifacts are missing:\"\n"
            "    sed 's/^/- /' /tmp/lumina-missing-artifacts.txt\n"
            "    echo\n"
            "    echo \"Only create or fix the missing artifact files. Do not restart the full task unless required to reconstruct the file contents.\"\n"
            "    echo \"Before you finish, verify each listed path exists and has the expected content.\"\n"
            "    echo\n"
            "    echo \"Original instruction:\"\n"
            "    cat /tmp/lumina-task.txt\n"
            "  } > /tmp/lumina-repair.txt\n"
            "  lumina --bare --yolo --harness-mode terminal-bench -p \"$(cat /tmp/lumina-repair.txt)\" || repair_status=$?\n"
            "  python3 /tmp/lumina-artifact-check.py repair \"${repair_status}\" || true\n"
            "fi\n"
            "ps -eo pid,ppid,pcpu,pmem,stat,comm,args > /logs/lumina-processes.txt 2>/dev/null || true\n"
            "final_status=${lumina_status}\n"
            "if [ \"${repair_ran}\" = \"1\" ]; then\n"
            "  if [ \"${repair_status}\" = \"0\" ]; then final_status=0; else final_status=${repair_status}; fi\n"
            "fi\n"
            "python3 /tmp/lumina-artifact-check.py final \"${final_status}\" || true\n"
            "echo \"LUMINA_EXIT_STATUS=${lumina_status}\"\n"
            "if [ \"${repair_ran}\" = \"1\" ]; then echo \"LUMINA_REPAIR_STATUS=${repair_status}\"; fi\n"
            "tmux wait -S done\n"
            "exit \"${final_status}\""
        )
        # The heredoc preserves the raw instruction; escaped_instruction is kept in
        # the generated command's shell context for easier debugging if needed.
        _ = escaped_instruction
        return [
            TerminalCommand(
                command=command,
                min_timeout_sec=1.0,
                max_timeout_sec=1800.0,
                block=True,
                append_enter=True,
            )
        ]

    def perform_task(
        self,
        instruction: str,
        session: TmuxSession,
        logging_dir: Path | None = None,
    ) -> AgentResult:
        rendered_instruction = self._render_instruction(instruction)
        if logging_dir is not None:
            logging_dir.mkdir(parents=True, exist_ok=True)
            (logging_dir / "lumina-instruction.txt").write_text(rendered_instruction)
        return super().perform_task(rendered_instruction, session, logging_dir)
