"""Harbor adapter: runs fuse as shipped, with bash directly in the task container."""
import os
import shlex
from pathlib import Path

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template

BIN_DIR = Path(os.environ.get("FUSE_BENCH_BIN", Path(__file__).resolve().parent / ".bin"))


class FuseAgent(BaseInstalledAgent):
    @staticmethod
    def name() -> str:
        return "fuse"

    def get_version_command(self) -> str | None:
        return "/usr/local/bin/fuse version | head -1"

    async def install(self, environment) -> None:
        arch = (await environment.exec("uname -m")).stdout.strip()
        goarch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[arch]
        await environment.upload_file(BIN_DIR / f"fuse-linux-{goarch}", "/usr/local/bin/fuse")
        await self.exec_as_root(environment, "chmod 755 /usr/local/bin/fuse")

    @with_prompt_template
    async def run(self, instruction, environment, context) -> None:
        env = self._extra_env
        model = self.model_name.split("/", 1)[1]
        # The task container is the sandbox, so bash runs directly in it. No output cap
        # or other tuning is set here unless run.py --fuse-option asks: by default this
        # measures fuse's defaults.
        config = (
            "gateway:\n"
            f"  url: {env['FUSE_BENCH_GATEWAY']}\n"
            f"  key: {env['FUSE_BENCH_KEY']}\n"
            "models:\n"
            "  baseline:\n"
            f"    id: {model}\n"
        )
        # run.py --fuse-option: model settings under test, "key: value" pairs joined by ";".
        for opt in filter(None, env.get("FUSE_BENCH_MODEL_OPTS", "").split(";")):
            config += f"    {opt}\n"
        cmd = (
            'mkdir -p "$HOME/.fuse" && '
            f'printf %s {shlex.quote(config)} > "$HOME/.fuse/config.yml" && '
            "printf 'handler: host\\n' > \"$HOME/.fuse/sandbox.local.yml\"; "
            "/usr/local/bin/fuse --model baseline --approve-all --trace /logs/agent/trace.jsonl "
            f"{shlex.quote(instruction)} > /logs/agent/fuse.txt 2>&1; "
            'echo "fuse exit $?" >> /logs/agent/fuse.txt'
        )
        await self.exec_as_agent(environment, cmd)

    def populate_context_post_run(self, context) -> None:
        return None
