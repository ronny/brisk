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

export interface Settings {
	bin: string
	mode: Mode
	base?: string
	/** Ask a person before Ship on High or unassessed changes in this project. */
	confirmShip: boolean
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
 * - amp.brisk.confirmShipOnHigh: true for every project, or a list of project
 *   roots to turn it on for.
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

	const confirm = c['amp.brisk.confirmShipOnHigh']
	const confirmShip =
		confirm === true ||
		(Array.isArray(confirm) &&
			confirm.some((p) => typeof p === 'string' && real(p.startsWith('~/') ? join(home, p.slice(2)) : p) === real(root)))
	return { bin, mode, base: str('base'), confirmShip }
}
