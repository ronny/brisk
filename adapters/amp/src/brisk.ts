// Runs the brisk binary and, for the host backend, answers its questions
// with a decision model. See README.md in the repository root for the
// contract and schema/ for the documents.

import { execFile } from 'node:child_process'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

export type Tier = 'low' | 'medium' | 'high'

export interface Evidence {
	path: string
	start_line?: number
	end_line?: number
	symbol?: string
}

export interface Reason {
	text: string
	source: string
	id: string
	tier: Tier
	evidence?: Evidence[]
}

export interface Assessment {
	schema: 'brisk.assessment/v1'
	tier: Tier
	floor: Tier
	impact: number
	uncertainty: number
	action: string
	reasons: Reason[]
	stats: { files: number; additions: number; deletions: number }
	provenance: {
		brisk_version: string
		backend: 'openrouter' | 'host' | 'rules-only'
		model?: string
		degraded?: string
		diff_hash: string
		[k: string]: unknown
	}
}

interface HostRequest {
	schema: 'brisk.host-request/v1'
	request_id: string
	model: string
	state: unknown
	questions: Record<string, unknown>
	floor_assessment: Assessment
}

/** Answers the questions of a host request, like amp.ai.decide. */
export type Decide = (req: {
	model: string
	state: unknown
	questions: Record<string, unknown>
}) => Promise<{ model: string; answers: Record<string, unknown> }>

/**
 * How to answer the model questions:
 * - `amp`: through Decide (amp.ai.decide), falling back to rules only.
 * - `binary`: let brisk do it (OpenRouter when OPENROUTER_API_KEY is set).
 * - `rules-only`: no model.
 */
export type Mode = 'amp' | 'binary' | 'rules-only'

export interface RunOptions {
	bin: string
	cwd: string
	mode: Mode
	base?: string
	session?: unknown
	decide?: Decide
	timeoutMs?: number
	env?: Record<string, string | undefined>
}

export interface RunResult {
	assessment: Assessment
	/** Why the model was not used, when it was meant to be. */
	degraded?: string
}

export class BriskError extends Error {}

function exec(bin: string, args: string[], o: RunOptions): Promise<string> {
	return new Promise((resolve, reject) => {
		execFile(
			bin,
			args,
			{ cwd: o.cwd, timeout: o.timeoutMs ?? 60_000, maxBuffer: 32 << 20, env: o.env ?? process.env },
			(err, stdout, stderr) => {
				// Exit code 3 means "tier at or above --fail-on"; brisk still prints the result.
				if (err && !(typeof err.code === 'number' && err.code === 3)) {
					const msg = String(stderr || err.message).trim().split('\n').slice(-3).join(' ')
					reject(new BriskError(`brisk ${args[0]} failed: ${msg}`))
					return
				}
				resolve(stdout)
			},
		)
	})
}

function parse(stdout: string): Assessment | HostRequest {
	let doc: unknown
	try {
		doc = JSON.parse(stdout)
	} catch {
		throw new BriskError(`brisk printed something that is not JSON: ${stdout.slice(0, 200)}`)
	}
	const schema = (doc as { schema?: string })?.schema
	if (schema !== 'brisk.assessment/v1' && schema !== 'brisk.host-request/v1') {
		throw new BriskError(`brisk printed an unknown document (schema ${JSON.stringify(schema)})`)
	}
	return doc as Assessment | HostRequest
}

function errorText(e: unknown): string {
	return e instanceof Error ? e.message : String(e)
}

/** assess runs brisk on the working tree in o.cwd and returns its assessment. */
export async function assess(o: RunOptions): Promise<RunResult> {
	const dir = await mkdtemp(join(tmpdir(), 'brisk-amp-'))
	try {
		const args = ['assess', '--format', 'json']
		if (o.base) args.push('--base', o.base)
		if (o.session) {
			const f = join(dir, 'session.json')
			await writeFile(f, JSON.stringify(o.session))
			args.push('--session', f)
		}
		const backend = { amp: 'host', binary: 'auto', 'rules-only': 'rules-only' }[o.mode]
		const first = parse(await exec(o.bin, [...args, '--backend', backend], o))
		if (first.schema === 'brisk.assessment/v1') return { assessment: first }
		if (!o.decide) throw new BriskError('brisk asked questions but no decision model is available')

		let answers
		try {
			answers = await o.decide({ model: first.model, state: first.state, questions: first.questions })
		} catch (e) {
			return { assessment: first.floor_assessment, degraded: `Amp decision model failed: ${errorText(e)}` }
		}
		const f = join(dir, 'answers.json')
		await writeFile(
			f,
			JSON.stringify({
				schema: 'brisk.host-answers/v1',
				request_id: first.request_id,
				model: answers.model,
				answers: answers.answers,
			}),
		)
		const final = parse(await exec(o.bin, [...args, '--answers', f], o))
		if (final.schema !== 'brisk.assessment/v1') throw new BriskError('brisk asked questions again after answers')
		return { assessment: final }
	} finally {
		await rm(dir, { recursive: true, force: true })
	}
}
