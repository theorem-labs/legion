from __future__ import annotations

import json
from pathlib import Path

import pytest
from conftest import FIXTURE

from cgraph import commands, config


def write(tmp_path: Path, text: str) -> Path:
    path = tmp_path / "cgraph.toml"
    path.write_text(text)
    return path


MINIMAL = """
[inputs]
c_tree = "tree"
image = "vmlinux"
system_map = "System.map"
[parts.one]
files = ["a.c"]
[c]
flags = ["-nostdinc"]
"""


def test_fixture_configuration_loads() -> None:
    loaded = config.load(FIXTURE / "cgraph.toml")
    assert loaded.inputs.c_tree == FIXTURE.resolve()
    assert loaded.parts["both"] == ["src/bags.c", "src/store.c"]
    assert loaded.vocabulary.membership == {"list_insert": "list", "hash_insert": "hash"}
    assert loaded.vocabulary.calls.action("up_write") == "unlock"
    assert loaded.vocabulary.calls.action("down_write") == "lock"
    assert loaded.vocabulary.calls.action("obj_free") == "free"
    assert loaded.topic_prefix == "notifications.example."


@pytest.mark.parametrize(
    ("change", "message"),
    [
        ('image = "vmlinux"\n', "inputs.image is required"),
        ('[parts.one]\nfiles = ["a.c"]\n', "configuration.parts is required"),
        ('flags = ["-nostdinc"]\n', "c.flags is required"),
    ],
)
def test_missing_key_names_the_key(tmp_path: Path, change: str, message: str) -> None:
    with pytest.raises(config.ConfigError, match=message.replace(".", r"\.")):
        config.load(write(tmp_path, MINIMAL.replace(change, "")))


def test_unknown_key_is_refused(tmp_path: Path) -> None:
    with pytest.raises(config.ConfigError, match=r"c has unknown key colour"):
        config.load(write(tmp_path, MINIMAL + 'colour = "blue"\n'))


def test_bad_regex_names_the_key(tmp_path: Path) -> None:
    with pytest.raises(config.ConfigError, match=r"c\.calls\.lock is not a valid regular expression"):
        config.load(write(tmp_path, MINIMAL + '[c.calls]\nlock = "("\n'))


def test_all_files_needs_a_compilation_database(tmp_path: Path) -> None:
    with pytest.raises(config.ConfigError, match=r'files = "all" needs inputs\.compile_commands'):
        config.load(write(tmp_path, MINIMAL.replace('files = ["a.c"]', 'files = "all"')))


def test_empty_part_is_refused(tmp_path: Path) -> None:
    with pytest.raises(config.ConfigError, match=r"parts\.one\.files must be a non-empty list"):
        config.load(write(tmp_path, MINIMAL.replace('files = ["a.c"]', "files = []")))


def test_root_overrides_the_configured_root(tmp_path: Path) -> None:
    loaded = config.load(write(tmp_path, MINIMAL), root=tmp_path / "elsewhere")
    assert loaded.inputs.image == (tmp_path / "elsewhere" / "vmlinux").resolve()


def test_all_files_come_from_the_compilation_database(tmp_path: Path) -> None:
    tree = tmp_path / "tree"
    (tree / "lib").mkdir(parents=True)
    for name in ("lib/a.c", "lib/b.c"):
        (tree / name).write_text("int x;\n")
    (tree / "lib/c.S").write_text("")
    database = [
        {
            "directory": str(tree),
            "file": "lib/a.c",
            "command": "clang -Wp,-MMD,x.d -nostdinc -I./include -include ./include/k.h -D__KERNEL__ -O2 -Wall "
            "--target=riscv64-linux-gnu -march=rv64imac -mabi=lp64 -fno-PIE -fms-extensions -c -o lib/a.o lib/a.c",
        },
        {"directory": str(tree), "file": str(tree / "lib/b.c"), "arguments": ["clang", "-DX=1", "-c", "lib/b.c"]},
        {"directory": str(tree), "file": "lib/c.S", "command": "clang -c lib/c.S"},
    ]
    (tree / "compile_commands.json").write_text(json.dumps(database))
    text = MINIMAL.replace('files = ["a.c"]', 'files = "all"').replace('[c]\nflags = ["-nostdinc"]\n', "")
    text = text.replace('c_tree = "tree"', 'c_tree = "tree"\ncompile_commands = "tree/compile_commands.json"')
    loaded = config.load(write(tmp_path, text))
    loaded_database = commands.load_database(tree / "compile_commands.json", tree)
    assert config.part_files(loaded, "one", loaded_database) == ["lib/a.c", "lib/b.c"]
    arguments = loaded_database["lib/a.c"]
    assert arguments[:4] == ["-nostdinc", f"-I{tree}/include", "-include", f"{tree}/include/k.h"]
    assert "-D__KERNEL__" in arguments and "--target=riscv64-linux-gnu" in arguments
    assert "-march=rv64imac" in arguments and "-fms-extensions" in arguments
    assert not any(item.startswith(("-O", "-Wp", "-Wall", "-c", "-o", "-fno-PIE")) for item in arguments[:-1])
    assert loaded_database["lib/b.c"] == ["-DX=1", "-Wno-everything"]


def test_argument_rewrites_replace_or_drop_what_the_parser_refuses(tmp_path: Path) -> None:
    text = MINIMAL + '[c.argument_rewrites]\n"-march=.*" = "-march=rv64imac"\n"-mno-save-restore" = ""\n'
    loaded = config.load(write(tmp_path, text))
    arguments = ["-march=rv64imac_zacas_zabha", "-mno-save-restore", "-DX"]
    assert commands.rewrite(arguments, loaded.argument_rewrites) == ["-march=rv64imac", "-DX"]


def test_bad_rewrite_pattern_names_the_key(tmp_path: Path) -> None:
    with pytest.raises(config.ConfigError, match=r"c\.argument_rewrites key \( is not a valid regular expression"):
        config.load(write(tmp_path, MINIMAL + '[c.argument_rewrites]\n"(" = ""\n'))


def test_part_file_missing_from_the_database_is_refused(tmp_path: Path) -> None:
    loaded = config.load(write(tmp_path, MINIMAL))
    with pytest.raises(config.ConfigError, match=r"parts\.one\.files names a\.c"):
        config.part_files(loaded, "one", {"b.c": []})
