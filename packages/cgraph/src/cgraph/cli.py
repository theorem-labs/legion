"""cgraph build --config FILE --part NAME --out DIR [--cache DIR] [--root DIR] [--jobs N] [--json]

Exit status: 0 the graph is built and every hard check passes; 1 a hard check failed (the outputs
are written); 2 a usage or configuration error; 3 inputs cannot be read or an internal error occurs;
4 a worker process died.
"""

from __future__ import annotations

import argparse
import json
import sys
import traceback
from pathlib import Path

from . import config as configuration
from . import pipeline
from .errors import InputError, WorkerDied


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="cgraph", description=__doc__.split("\n\n")[0])
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("build", help="build the graph, its checks, owners and topics for one part")
    build.add_argument("--config", required=True, type=Path)
    build.add_argument("--part", required=True)
    build.add_argument("--out", required=True, type=Path)
    build.add_argument(
        "--cache", type=Path, help="keep each unit's facts here and reuse those whose inputs did not change"
    )
    build.add_argument("--root", type=Path, help="resolve the configuration's input paths against this directory")
    build.add_argument("--jobs", type=int, default=pipeline.default_jobs())
    build.add_argument("--json", action="store_true", help="print the summary as JSON")
    arguments = parser.parse_args(argv)
    try:
        loaded = configuration.load(arguments.config, arguments.root)
        result = pipeline.run(loaded, arguments.part, arguments.out, arguments.cache, max(1, arguments.jobs))
    except configuration.ConfigError as error:
        print(f"cgraph: {error}", file=sys.stderr)
        return 2
    except WorkerDied as error:
        print(f"cgraph: {error}", file=sys.stderr)
        return 4
    except (InputError, OSError) as error:
        print(f"cgraph: {error}", file=sys.stderr)
        return 3
    except Exception as error:
        print(f"cgraph: internal error: {error}", file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        return 3
    summary = {
        "passes": result.passes,
        "counts": result.counts,
        "parsed": len(result.parsed),
        "reused": len(result.reused),
        "hard_checks": [
            {
                "name": item["name"],
                "ok": item["ok"],
                "count": item.get("count", 0),
                "detail": item["detail"],
            }
            for item in result.hard_checks
        ],
        "outputs": result.outputs,
    }
    if arguments.json:
        print(json.dumps(summary, indent=2, sort_keys=True))
    else:
        print(
            f"{'passes' if result.passes else 'FAILS'}: {result.counts['nodes']} nodes, {result.counts['edges']} "
            f"edges, {result.counts['owners']} owners; parsed {len(result.parsed)} of {result.counts['files']} files"
        )
        for item in result.hard_checks:
            if not item["ok"]:
                print(f"  FAIL {item['name']}: {item['detail']}")
    return 0 if result.passes else 1


if __name__ == "__main__":
    sys.exit(main())
