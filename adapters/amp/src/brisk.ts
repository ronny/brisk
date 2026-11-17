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

/**
 * One area of brisk's review plan. `source` brisk: area and check text from
 * brisk's catalog; config: from the project's base .brisk.yaml. `score` is
 * Jev's 0-3 relevance, absent when untailored.
 */
export interface ReviewArea {
	id: string
	area: string
	source: 'brisk' | 'config'
	required?: boolean
	score?: number
	evidence?: Evidence[]
	checks: { id: string; text: string }[]
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
	/** Whether a further agent review is recommended, separate from the tier. Older binaries omit it. */
	agent_review?: { recommended: boolean; reason: string; plan?: ReviewArea[] }
	provenance: {
		brisk_version: string
		backend: 'openrouter' | 'host' | 'rules-only'
		model?: string
		degraded?: string
		diff_hash: string
		/** The preset the project's base .brisk.yaml chose, if any. */
		preset?: string
		/** Whether Jev's area scores shaped the review plan. */
		review_plan?: { status?: 'tailored' | 'not_asked' | 'pending' | 'failed'; tailored?: boolean; error?: string; [k: string]: unknown }
		[k: string]: unknown
	}
}

interface HostRequest {
	schema: 'brisk.host-request/v1'
	request_id: string
	model: string
	state: unknown
	questions: Record<string, unknown>
	/** One relevance question per review area; answered separately, see README. */
	review_questions?: Record<string, unknown>
	/** When to answer review_questions: only once a review is recommended (default), or always. */
	review_when?: 'recommended' | 'always'
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

/** How long the optional review-plan answers may add to an assessment. */
const REVIEW_TIMEOUT_MS = 5_000

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

		// The review questions (one relevance score per plan area) go in their
		// own call so they cannot change the calibrated answers. With
		// review_when "recommended" they are asked only when the answers lead to
		// a recommended review; "always" asks them alongside the main call.
		// Without their answers brisk keeps its full plan, so a failure is
		// reported as review_error, not as an error.
		const decide = o.decide
		const when = first.review_questions ? (first.review_when === 'always' ? 'always' : 'recommended') : undefined
		const askReview = (): Promise<{ answers?: Record<string, unknown>; error?: string }> => {
			let timer: ReturnType<typeof setTimeout> | undefined
			return Promise.race([
				decide({ model: first.model, state: first.state, questions: first.review_questions! }),
				new Promise<never>((_, reject) => {
					timer = setTimeout(() => reject(new Error(`timed out after ${REVIEW_TIMEOUT_MS / 1000} s`)), REVIEW_TIMEOUT_MS)
				}),
			])
				.then(
					(r) => {
						// brisk counts partial answers as failed; say which are missing.
						const missing = Object.keys(first.review_questions!).filter((k) => !(k in (r.answers ?? {})))
						return missing.length ? { error: `no answers for ${missing.join(', ')}` } : { answers: r.answers }
					},
					(e) => ({ error: errorText(e) }),
				)
				.finally(() => clearTimeout(timer))
		}
		const early = when === 'always' ? askReview() : undefined
		let answers
		try {
			answers = await decide({ model: first.model, state: first.state, questions: first.questions })
		} catch (e) {
			return { assessment: first.floor_assessment, degraded: `Amp decision model failed: ${errorText(e)}` }
		}
		const f = join(dir, 'answers.json')
		const finish = async (review?: { answers?: Record<string, unknown>; error?: string }): Promise<Assessment> => {
			await writeFile(
				f,
				JSON.stringify({
					schema: 'brisk.host-answers/v1',
					request_id: first.request_id,
					model: answers.model,
					answers: answers.answers,
					...(review?.answers ? { review_answers: review.answers } : {}),
					...(review?.error ? { review_error: review.error } : {}),
				}),
			)
			const doc = parse(await exec(o.bin, [...args, '--answers', f], o))
			if (doc.schema !== 'brisk.assessment/v1') throw new BriskError('brisk asked questions again after answers')
			return doc
		}
		let final: Assessment
		if (early) final = await finish(await early)
		else {
			final = await finish()
			if (when === 'recommended' && final.agent_review?.recommended) {
				// Tailoring is optional: if this run fails (for example brisk
				// refuses because the files changed meanwhile), keep the
				// assessment already made.
				const untailored = final
				final = await finish(await askReview()).catch(() => untailored)
			}
		}
		return { assessment: final }
	} finally {
		await rm(dir, { recursive: true, force: true })
	}
}
