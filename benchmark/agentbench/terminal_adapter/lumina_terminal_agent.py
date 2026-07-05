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
            "lumina --bare --yolo -p \"$(cat /tmp/lumina-task.txt)\""
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
