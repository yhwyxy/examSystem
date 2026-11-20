"""Run native PostgreSQL clients with explicit connection fields, no URI argv."""
from __future__ import annotations

import os
import subprocess
import sys
from urllib.parse import parse_qsl, unquote, urlsplit


def connection_env(uri: str, base: dict[str, str] | None = None) -> dict[str, str]:
    parts = urlsplit(uri)
    if parts.scheme not in {"postgres", "postgresql"} or not parts.hostname or not parts.username or not parts.path.strip("/"):
        raise ValueError("explicit PostgreSQL URI fields are required")
    source = os.environ if base is None else base
    allowed = {"PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "SYSTEMROOT", "TEMP", "TMPDIR"}
    env = {key: source[key] for key in allowed if key in source}
    env.update(PGHOST=unquote(parts.hostname), PGPORT=str(parts.port or 5432),
               PGUSER=unquote(parts.username), PGDATABASE=unquote(parts.path.lstrip("/")), PGCONNECT_TIMEOUT="5")
    if parts.password is not None:
        env["PGPASSWORD"] = unquote(parts.password)
    parameters = {"sslmode": "PGSSLMODE", "sslrootcert": "PGSSLROOTCERT", "sslcert": "PGSSLCERT",
                  "sslkey": "PGSSLKEY", "connect_timeout": "PGCONNECT_TIMEOUT", "application_name": "PGAPPNAME"}
    for key, value in parse_qsl(parts.query):
        if key not in parameters:
            raise ValueError("unsupported PostgreSQL URI parameter")
        env[parameters[key]] = value
    return env


def main() -> int:
    if len(sys.argv) < 2:
        return 2
    try:
        env = connection_env(os.environ.get("BACKUP_DATABASE_URL", ""))
        return subprocess.run(sys.argv[1:], env=env, check=False).returncode
    except (ValueError, OSError):
        print("invalid native PostgreSQL connection or unavailable client", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
