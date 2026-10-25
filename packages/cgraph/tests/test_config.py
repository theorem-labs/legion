from __future__ import annotations

import json
from pathlib import Path

import pytest
from conftest import FIXTURE, build

from cgraph import clangfacts, cli, commands, config
from cgraph.errors import ExtractionError


def write(tmp_path: Path, text: str) -> Path:
    path = tmp_path / "cgraph.toml"
    path.write_text(text)
    return path


def reproducible_outputs(out: Path) -> dict[str, bytes]:
    """Build metadata records cache use; the graph outputs must not depend on it."""
    return {path.name: path.read_bytes() for path in out.iterdir() if path.name != "build.json"}


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


@pytest.mark.parametrize("flag", ["-I{bar}", "-I{c_tree", "-I{0}"])
def test_a_flag_with_an_unknown_placeholder_names_the_key(tmp_path: Path, flag: str) -> None:
    """LegionR6ReviewClaude finding on exit status: a bad c.flags placeholder is a configuration
    error (2), not an input that cannot be read (3) and no traceback."""
    with pytest.raises(config.ConfigError, match=r"c\.flags"):
        config.load(write(tmp_path, MINIMAL.replace('flags = ["-nostdinc"]', f'flags = ["{flag}"]')))


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


def test_allocate_rule_emits_the_allocate_access_kind(tmp_path: Path) -> None:
    loaded = config.load(write(tmp_path, MINIMAL + '[c.calls]\nallocate = "alloc"\n'))
    assert loaded.vocabulary.calls.action("obj_alloc") == "allocate"


def test_legacy_alloc_rule_is_refused(tmp_path: Path) -> None:
    with pytest.raises(config.ConfigError, match=r"c\.calls has unknown key alloc"):
        config.load(write(tmp_path, MINIMAL + '[c.calls]\nalloc = "alloc"\n'))


TRAP_TABLE = """[image.trap_table]
section = "__bug_table"
entry_size = 8
address_offset = 0
address_size = 4
relative = true
flags_offset = 4
flags_size = 2
resume_flags = 1
"""


def test_trap_table_is_read(tmp_path: Path) -> None:
    loaded = config.load(write(tmp_path, MINIMAL + TRAP_TABLE))
    assert loaded.image.trap_table == config.TrapTable(
        section="__bug_table",
        entry_size=8,
        address_offset=0,
        address_size=4,
        relative=True,
        flags_offset=4,
        flags_size=2,
        resume_flags=1,
    )
    assert config.load(write(tmp_path, MINIMAL)).image.trap_table is None


@pytest.mark.parametrize(
    ("change", "replacement", "message"),
    [
        ("flags_size = 2\n", "", r"image\.trap_table\.flags_size is required"),
        (
            "flags_offset = 4\n",
            "flags_offset = 7\n",
            r"image\.trap_table's flags field ends at byte 9, past entry_size 8",
        ),
        ("address_size = 4\n", "address_size = 3\n", r"image\.trap_table\.address_size must be 4 or 8"),
        ("flags_size = 2\n", "flags_size = 3\n", r"image\.trap_table\.flags_size must be 1, 2, 4 or 8"),
        ("resume_flags = 1\n", "resume_flags = 0\n", r"image\.trap_table\.resume_flags must be a nonzero bit mask"),
        ("entry_size = 8\n", "entry_size = true\n", r"image\.trap_table\.entry_size must be an integer"),
        ("relative = true\n", 'relative = "yes"\n', r"image\.trap_table\.relative must be a boolean"),
        ('section = "__bug_table"\n', 'section = ""\n', r"image\.trap_table\.section must name a section"),
        ("resume_flags = 1\n", 'resume_flags = 1\ncolour = "blue"\n', r"image\.trap_table has unknown key colour"),
    ],
)
def test_trap_table_is_checked(tmp_path: Path, change: str, replacement: str, message: str) -> None:
    with pytest.raises(config.ConfigError, match=message):
        config.load(write(tmp_path, MINIMAL + TRAP_TABLE.replace(change, replacement)))


PATCH_TABLE = """[image.patch_table]
section = ".alternative"
entry_size = 16
site_offset = 0
replacement_offset = 4
address_size = 4
relative = true
length_offset = 10
length_size = 2
"""


def test_patch_table_is_read(tmp_path: Path) -> None:
    loaded = config.load(write(tmp_path, MINIMAL + PATCH_TABLE))
    assert loaded.image.patch_table == config.PatchTable(
        section=".alternative",
        entry_size=16,
        site_offset=0,
        replacement_offset=4,
        address_size=4,
        relative=True,
        length_offset=10,
        length_size=2,
    )
    assert config.load(write(tmp_path, MINIMAL)).image.patch_table is None


@pytest.mark.parametrize(
    ("change", "replacement", "message"),
    [
        ("length_size = 2\n", "", r"image\.patch_table\.length_size is required"),
        (
            "replacement_offset = 4\n",
            "replacement_offset = 13\n",
            r"image\.patch_table's replacement field ends at byte 17, past entry_size 16",
        ),
        (
            "length_offset = 10\n",
            "length_offset = 15\n",
            r"image\.patch_table's length field ends at byte 17, past entry_size 16",
        ),
        ("address_size = 4\n", "address_size = 2\n", r"image\.patch_table\.address_size must be 4 or 8"),
        ("length_size = 2\n", "length_size = 3\n", r"image\.patch_table\.length_size must be 1, 2, 4 or 8"),
        ("site_offset = 0\n", "site_offset = -1\n", r"image\.patch_table\.site_offset must be an integer"),
        ("relative = true\n", "relative = 1\n", r"image\.patch_table\.relative must be a boolean"),
        ('section = ".alternative"\n', 'section = ""\n', r"image\.patch_table\.section must name a section"),
        ("length_size = 2\n", "length_size = 2\nvendor_offset = 8\n", r"image\.patch_table has unknown key vendor"),
    ],
)
def test_patch_table_is_checked(tmp_path: Path, change: str, replacement: str, message: str) -> None:
    with pytest.raises(config.ConfigError, match=message):
        config.load(write(tmp_path, MINIMAL + PATCH_TABLE.replace(change, replacement)))


def test_new_shadowing_header_reparses_all_units_that_include_it(copy: Path, tmp_path: Path) -> None:
    cache = tmp_path / "cache"
    before, incremental_out, full_out = tmp_path / "before", tmp_path / "incremental", tmp_path / "full"
    build(copy, "both", before, cache)
    header = (copy / "include" / "mini.h").read_text()
    (copy / "include" / "override" / "mini.h").write_text(
        header.replace("#endif\n", "struct shadow_cache_probe {\n\tint field;\n};\n\n#endif\n")
    )
    incremental = build(copy, "both", incremental_out, cache)
    full = build(copy, "both", full_out, tmp_path / "empty-cache")
    assert incremental.parsed == ["src/bags.c", "src/store.c"]
    assert reproducible_outputs(incremental_out) == reproducible_outputs(full_out)
    assert full.parsed == ["src/bags.c", "src/store.c"]


def test_bags_header_change_reparses_only_bags(copy: Path, tmp_path: Path) -> None:
    cache = tmp_path / "cache"
    before, incremental_out, full_out = tmp_path / "before", tmp_path / "incremental", tmp_path / "full"
    build(copy, "both", before, cache)
    header = copy / "include" / "bags.h"
    header.write_text(header.read_text() + "\nint bags_header_changed(void);\n")
    incremental = build(copy, "both", incremental_out, cache)
    full = build(copy, "both", full_out, tmp_path / "empty-cache")
    assert incremental.parsed == ["src/bags.c"]
    assert incremental.reused == ["src/store.c"]
    assert reproducible_outputs(incremental_out) == reproducible_outputs(full_out)
    assert full.parsed == ["src/bags.c", "src/store.c"]


def test_extraction_error_exits_three_with_its_unit_name(
    copy: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    def fail(source: str, *_: object) -> dict[str, object]:
        raise ExtractionError(f"{source}: src/store.c:1: cannot read tokens")

    monkeypatch.setattr(clangfacts, "extract", fail)
    assert (
        cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "store", "--out", str(tmp_path / "out")])
        == 3
    )
    assert "src/store.c" in capsys.readouterr().err
