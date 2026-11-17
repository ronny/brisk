// brisk settings come only from files the repository cannot write: the
// user's Amp settings and the machine's managed settings. Amp's merged
// configuration also includes the repository's .amp/settings.json, which a
// change under assessment could edit to pick the binary that rates it, lower
// the backend, or turn the Ship confirmation off.

import { realpathSync } from 'node:fs'
import { access, constants, readFile } from 'node:fs/promises'
import { homedir, platform } from 'node:os'
import { delimiter, isAbsolute, join, relative, resolve } from 'node:path'
import type { Mode } from './brisk.ts'

/**
 * What the plugin does when brisk recommends an agent review, from least to
 * most automatic:
 * - off: nothing.
 * - suggest: a quick action and a line in the Ship prompt.
 * - before-ship: also tells the Ship and Push to Branch turn to run the review first.
 * - after-turn: also starts the review as soon as the agent's turn ends.
 */
export const REVIEW_MODES = ['off', 'suggest', 'before-ship', 'after-turn'] as const
export type ReviewMode = (typeof REVIEW_MODES)[number]

/**
 * Plugin defaults per preset. The names match brisk's .brisk.yaml presets.
 * Explicit settings override them.
 */
export const PRESETS: Record<string, { agentReview: ReviewMode; confirmShip: boolean }> = {
	// Nobody reviews the agent's work, so the agent reviews it itself.
	autonomous: { agentReview: 'after-turn', confirmShip: false },
	// People review pull requests; brisk points them at the risky parts.
	reviewed: { agentReview: 'suggest', confirmShip: false },
	// Review before every risky Ship, and a person confirms High.
	strict: { agentReview: 'before-ship', confirmShip: true },
}
const DEFAULT_PRESET = 'reviewed'

export interface Settings {
	bin: string
	mode: Mode
	base?: string
	/** The trusted preset in effect. */
	preset: string
	/** Ask a person before Ship on High or unassessed changes in this project. */
	confirmShip: boolean
	agentReview: ReviewMode
}

/**
 * reviewMode is what to do about a recommended agent review. The project's
 * own preset (from its base .brisk.yaml, reported by brisk) can make this
 * more automatic than the user's settings, up to before-ship, never less,
 * and not when the user turned agent review off.
 */
export function reviewMode(cfg: Settings, projectPreset?: string): ReviewMode {
	let project = projectPreset && Object.hasOwn(PRESETS, projectPreset) ? PRESETS[projectPreset].agentReview : undefined
	// off is the user's explicit opt-out, so the project cannot override it.
	if (!project || cfg.agentReview === 'off') return cfg.agentReview
	// Only the user can choose after-turn, so a cloned repository cannot
	// make the agent start turns on its own.
	if (project === 'after-turn') project = 'before-ship'
	return REVIEW_MODES.indexOf(project) > REVIEW_MODES.indexOf(cfg.agentReview) ? project : cfg.agentReview
}

/** Paths of the trusted settings files, lowest precedence first. */
export function trustedFiles(home = process.env.HOME || homedir(), os = platform()): string[] {
	const managed =
		os === 'darwin'
			? '/Library/Application Support/ampcode/managed-settings.json'
			: os === 'win32'
				? join(process.env.ProgramData ?? 'C:\\ProgramData', 'ampcode', 'managed-settings.json')
				: '/etc/ampcode/managed-settings.json'
	return [join(home, '.config', 'amp', 'settings.json'), join(home, '.config', 'amp', 'settings.jsonc'), managed]
}

/** stripJSONC removes comments and trailing commas outside strings. */
export function stripJSONC(s: string): string {
	let out = ''
	for (let i = 0; i < s.length; i++) {
		const c = s[i]
		if (c === '"') {
			let j = i + 1
			while (j < s.length && s[j] !== '"') j += s[j] === '\\' ? 2 : 1
			out += s.slice(i, j + 1)
			i = j
		} else if (c === '/' && s[i + 1] === '/') {
			while (i < s.length && s[i] !== '\n') i++
			out += '\n'
		} else if (c === '/' && s[i + 1] === '*') {
			i = s.indexOf('*/', i + 2)
			if (i < 0) break
			i++
		} else out += c
	}
	return out.replace(/,(\s*[}\]])/g, '$1')
}

/** readTrusted merges the trusted files that exist; later files win. */
export async function readTrusted(files = trustedFiles()): Promise<Record<string, unknown>> {
	const merged: Record<string, unknown> = {}
	for (const f of files) {
		let text: string
		try {
			text = await readFile(f, 'utf8')
		} catch {
			continue
		}
		try {
			Object.assign(merged, JSON.parse(stripJSONC(text)))
		} catch {
			// Unparseable: Amp rejects it too, so skip it.
		}
	}
	return merged
}

/** real resolves symlinks in p as far as it exists. */
function real(p: string): string {
	try {
		return realpathSync(p)
	} catch {
		return resolve(p)
	}
}

function inside(path: string, root: string): boolean {
	const r = relative(real(root), real(path))
	return r === '' || (!r.startsWith('..') && !isAbsolute(r))
}

/**
 * resolveBinary turns bin into an absolute path outside the project. A bare
 * name is looked up on PATH, skipping relative entries and entries inside the
 * project, which would otherwise let the change under assessment supply the
 * binary that rates it.
 */
export async function resolveBinary(bin: string, root: string, path = process.env.PATH ?? ''): Promise<string> {
	const candidates = isAbsolute(bin)
		? [bin]
		: path
				.split(delimiter)
				.filter((d) => d && isAbsolute(d) && !inside(d, root))
				.flatMap((d) => (platform() === 'win32' ? ['.exe', '.cmd', ''].map((e) => join(d, bin + e)) : [join(d, bin)]))
	for (const c of candidates) {
		if (inside(c, root)) continue
		try {
			await access(c, constants.X_OK)
			return c
		} catch {}
	}
	throw new Error(`${bin} not found outside the project; set amp.brisk.binary in user settings`)
}

/**
 * settingsFrom reads brisk's settings from trusted values (keys with the
 * `amp.` prefix) for the project at root.
 *
 * - amp.brisk.binary: a bare command name looked up on PATH, or an absolute
 *   (or ~/) path outside the project. Anything else falls back to `brisk`.
 * - amp.brisk.preset: autonomous, reviewed (default), or strict; sets the
 *   defaults of the two keys below.
 * - amp.brisk.confirmShipOnHigh: true for every project, false, or a list of
 *   project roots to turn it on for.
 * - amp.brisk.agentReview: one of REVIEW_MODES.
 */
export function settingsFrom(
	c: Record<string, unknown>,
	root: string,
	env: Record<string, string | undefined> = process.env,
	home = homedir(),
): Settings {
	const str = (k: string) => {
		const v = c[`amp.brisk.${k}`]
		return typeof v === 'string' && v ? v : undefined
	}
	let bin = str('binary') ?? 'brisk'
	if (bin.startsWith('~/')) bin = join(home, bin.slice(2))
	if (/[\\/]/.test(bin) && (!isAbsolute(bin) || inside(bin, root))) bin = 'brisk'

	const backend = str('backend') ?? 'auto'
	const mode: Mode =
		backend === 'rules-only'
			? 'rules-only'
			: backend === 'openrouter'
				? 'binary'
				: backend === 'amp'
					? 'amp'
					: env.OPENROUTER_API_KEY
						? 'binary'
						: 'amp'

	const preset = Object.hasOwn(PRESETS, str('preset') ?? '') ? str('preset')! : DEFAULT_PRESET
	const confirm = c['amp.brisk.confirmShipOnHigh']
	const confirmShip =
		confirm === undefined
			? PRESETS[preset].confirmShip
			: confirm === true ||
				(Array.isArray(confirm) &&
					confirm.some((p) => typeof p === 'string' && real(p.startsWith('~/') ? join(home, p.slice(2)) : p) === real(root)))
	const review = str('agentReview')
	const agentReview = (REVIEW_MODES as readonly string[]).includes(review ?? '') ? (review as ReviewMode) : PRESETS[preset].agentReview
	return { bin, mode, base: str('base'), preset, confirmShip, agentReview }
}
