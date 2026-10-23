import { ROLE_TOPIC_PREFIX } from "./subject";

/** A Legion issue key: a Dispatch key (`^[A-Z][A-Z0-9]*-[0-9]+$`, e.g. `LEGION-7`). Owner/repo
 * comes from `DaemonConfig`, never from this key — GitHub issues are never part of Legion's
 * lifecycle (see the T20 design). Not a template-literal union so it stays ergonomic as a plain
 * string through roles/state; every value at runtime matches `ISSUE_KEY_PATTERN`
 * (`legion-state.ts`). */
export type IssueKey = string;

export const LEGION_ROLES = [
  "architect",
  "planner",
  "implementer",
  "tester",
  "reviewer",
  "merger",
] as const;

export type LegionRole = (typeof LEGION_ROLES)[number];

// StandingOwnerRole is tree-independent: unlike LegionRole, it never names a Dispatch issue.
export const STANDING_OWNER_ROLE = "owner" as const;
export const LEGION_CLAIM_ROLES = [...LEGION_ROLES, STANDING_OWNER_ROLE] as const;
export type LegionClaimRole = (typeof LEGION_CLAIM_ROLES)[number];

export function isLegionClaimRole(role: string): role is LegionClaimRole {
  return LEGION_CLAIM_ROLES.some((candidate) => candidate === role);
}

export interface ParsedRoleToken {
  project: string;
  issue: IssueKey;
  role: LegionRole;
}

const ENVOY_ROLE_TOKEN = /^[a-z0-9][a-z0-9_-]*$/;
const PROJECT_TOKEN = /^[a-z0-9]+$/;
/** A Dispatch issue key: uppercase project, then a numeric suffix — `IssueKey`'s sole shape. */
export const DISPATCH_KEY_PATTERN = /^([A-Z][A-Z0-9]*)-([0-9]+)$/;
const DISPATCH_ROLE_PART =
  /^([a-z0-9]+)-([0-9]+)-(architect|planner|implementer|tester|reviewer|merger)$/;

export function isLegionRole(role: string): role is LegionRole {
  return LEGION_ROLES.some((candidate) => candidate === role);
}

export function isLegionProjectToken(project: string): boolean {
  return PROJECT_TOKEN.test(project);
}

/** The one rule that turns the operator-written `project` (`legion.yaml`'s value, `LEGION_ID`,
 * `sjawhar/legion`) into the Legion project token every role token, secret file, and
 * `LEGION_PROJECT` carries (`sjawharlegion`): lowercased, every non-alphanumeric dropped — the Go
 * daemon's `claim.ProjectToken` too. The TypeScript daemon, `legion controller start`'s
 * operator-side loader and the Go controller's plugin all apply it, so an operator's copied value
 * lands on the daemon's own controller token; a value that sanitizes to nothing is refused naming
 * `field`. */
export function legionProjectToken(value: string, field: string): string {
  const project = value.toLowerCase().replace(/[^a-z0-9]/g, "");
  if (!project) throw new Error(`${field} must include at least one alphanumeric character`);
  return project;
}

export function assertLegionProjectToken(project: string): void {
  if (!isLegionProjectToken(project)) {
    throw new Error(`Invalid Legion project token: ${project}`);
  }
}

export function roleToken(project: string, issue: IssueKey, role: LegionRole): string {
  assertLegionProjectToken(project);

  const dispatchMatch = DISPATCH_KEY_PATTERN.exec(issue);
  if (!dispatchMatch) throw new Error(`Invalid IssueKey: ${issue}`);
  const [, dispatchProject, number] = dispatchMatch;
  return `legion-${project}-${dispatchProject?.toLowerCase()}-${number}-${role}`;
}

export function sanitizeToken(part: string): string {
  const token = part
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");

  return token || "x";
}

export function controllerToken(project: string): string {
  assertLegionProjectToken(project);
  return `legion-${project}-controller`;
}

/** The graph node kinds an owner key may name, the part of a node id before its first colon —
 * the Go daemon's `claim.NodeKinds`. */
export const OWNER_NODE_KINDS = ["function", "struct", "global", "percpu", "subsystem"] as const;
/** A graph node id (`function:mnt_get_writers@fs/namespace.c`) or a named role (`librarian`) —
 * the Go daemon's `claim.IsOwnerKey`. */
const NODE_OWNER_KEY = new RegExp(`^(?:${OWNER_NODE_KINDS.join("|")}):\\S+$`);
const NAMED_OWNER_KEY = /^[a-z][a-z0-9-]*$/;
/** The bytes an owner name spells as `_` and one letter; every other byte outside `[a-z0-9-]` but
 * an uppercase letter is `_x` and two lowercase hex digits. */
const OWNER_ESCAPES: Record<string, string> = { _: "_", ".": "d", ":": "c", "/": "s", "@": "a" };
const OWNER_UNESCAPES: Record<string, string> = { _: "_", d: ".", c: ":", s: "/", a: "@" };
/** An owner token names files in the daemon's state directory, so it stays under NAME_MAX with
 * room for their suffixes — the Go daemon's `claim.MaxOwnerTokenLength`. */
const MAX_OWNER_TOKEN_LENGTH = 200;

function isOwnerKey(key: string): boolean {
  return NODE_OWNER_KEY.test(key) || NAMED_OWNER_KEY.test(key);
}

function literalOwnerByte(byte: number): boolean {
  return (byte >= 0x61 && byte <= 0x7a) || (byte >= 0x30 && byte <= 0x39) || byte === 0x2d;
}

/** The Envoy role token of a tree-independent standing owner, `legion-<project>-owner-<name>`,
 * byte for byte the Go daemon's `claim.OwnerToken`: name is the key's UTF-8 bytes with a-z, 0-9
 * and `-` as themselves, `_` as `__`, `.` as `_d`, `:` as `_c`, `/` as `_s`, `@` as `_a`, an
 * uppercase letter as `_u` and the letter lowercased, and any other byte as `_x` and two lowercase
 * hex digits, so two keys never share a token. */
export function standingOwnerToken(project: string, owner: string): string {
  assertLegionProjectToken(project);
  if (!isOwnerKey(owner)) {
    throw new Error(`Invalid standing owner: ${owner}`);
  }
  let name = "";
  for (const byte of new TextEncoder().encode(owner)) {
    const char = String.fromCharCode(byte);
    const escaped = OWNER_ESCAPES[char];
    if (literalOwnerByte(byte)) name += char;
    else if (escaped !== undefined) name += `_${escaped}`;
    else if (byte >= 0x41 && byte <= 0x5a) name += `_u${char.toLowerCase()}`;
    else name += `_x${byte.toString(16).padStart(2, "0")}`;
  }
  const token = `legion-${project}-owner-${name}`;
  if (token.length > MAX_OWNER_TOKEN_LENGTH) {
    throw new Error(
      `Invalid standing owner: ${owner} makes a ${token.length}-byte token, past ${MAX_OWNER_TOKEN_LENGTH}`
    );
  }
  return token;
}

/** The owner key a standing owner's token of project encodes — the Go daemon's `Token.OwnerKey`,
 * as strict as its `claim.DecodeOwnerName` — or undefined for a token that is no owner token of
 * project, or whose name no key encodes to. */
export function standingOwnerKey(project: string, token: string): string | undefined {
  const prefix = `legion-${project}-owner-`;
  if (!isLegionProjectToken(project) || !token.startsWith(prefix)) return undefined;
  const name = token.slice(prefix.length);
  const bytes: number[] = [];
  for (let i = 0; i < name.length; i += 1) {
    const byte = name.charCodeAt(i);
    if (literalOwnerByte(byte)) {
      bytes.push(byte);
      continue;
    }
    if (name[i] !== "_" || i + 1 >= name.length) return undefined;
    i += 1;
    const letterAfter = name[i] ?? "";
    if (letterAfter === "u") {
      const letter = name.charCodeAt(i + 1);
      if (!(letter >= 0x61 && letter <= 0x7a)) return undefined;
      i += 1;
      bytes.push(letter - 0x20);
    } else if (letterAfter === "x") {
      const hex = name.slice(i + 1, i + 3);
      if (!/^[0-9a-f]{2}$/.test(hex)) return undefined;
      const byte = Number.parseInt(hex, 16);
      // An escape of a byte with a shorter spelling is no key's name: refused, as Go refuses it.
      const shorter =
        literalOwnerByte(byte) ||
        OWNER_ESCAPES[String.fromCharCode(byte)] !== undefined ||
        (byte >= 0x41 && byte <= 0x5a);
      if (shorter) return undefined;
      i += 2;
      bytes.push(byte);
    } else {
      const unescaped = OWNER_UNESCAPES[letterAfter];
      if (unescaped === undefined) return undefined;
      bytes.push(unescaped.charCodeAt(0));
    }
  }
  let key: string;
  try {
    key = new TextDecoder("utf-8", { fatal: true }).decode(new Uint8Array(bytes));
  } catch {
    return undefined;
  }
  return isOwnerKey(key) ? key : undefined;
}

export function roleTopic(token: string): string {
  return `${ROLE_TOPIC_PREFIX}${token}`;
}

/** A Dispatch issue key encodes as `<project>-<number>-<role>`; its project component is already
 * `[A-Z][A-Z0-9]*`, so lowercasing it needs no further escaping. */
export function parseRoleToken(
  project: string,
  token: string
): ParsedRoleToken | { controller: true } | undefined {
  if (!isLegionProjectToken(project)) return undefined;

  const prefix = `legion-${project}-`;
  if (!ENVOY_ROLE_TOKEN.test(token) || !token.startsWith(prefix)) return undefined;

  const rest = token.slice(prefix.length);
  if (rest === "controller") return { controller: true };

  const dispatchMatch = DISPATCH_ROLE_PART.exec(rest);
  if (!dispatchMatch) return undefined;
  const [, dispatchProject, numberPart, role] = dispatchMatch;
  if (!dispatchProject || !numberPart || !role || !isLegionRole(role)) return undefined;
  return {
    project,
    issue: `${dispatchProject.toUpperCase()}-${numberPart}`,
    role,
  };
}
