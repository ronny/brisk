// brisk for Amp: rates the risk of a thread's code changes with the brisk
// binary and shows it as a thread label, a quick action, and a line in the
// Ship prompt. Advisory unless a project turns on brisk.confirmShipOnHigh.

import type { PluginAPI, PluginThread, QuickAction, ThreadMessage } from '@ampcode/plugin'
import { assess, type Decide, type Mode, type RunResult } from './src/brisk.ts'
import { labelsFor, reviewMessage, shipLine, summary } from './src/present.ts'
import { buildSession, turnsFromMessages } from './src/session.ts'

export const description =
	"Rates how risky the thread's code changes are (Low, Medium, High) with brisk. Labels the thread, adds a review quick action, and notes Medium and High risk in the Ship prompt."

/**
 * Settings, read from Amp's configuration. Set them in workspace (project)
 * settings to apply to one project only.
 *
 * - brisk.binary: path to the brisk binary. Default: `brisk` on PATH.
 * - brisk.backend: `auto` (default: OpenRouter when OPENROUTER_API_KEY is set,
 *   else Amp's Jev), `amp`, `openrouter`, or `rules-only`.
 * - brisk.base: base ref to compare against. Default: brisk's own choice.
 * - brisk.confirmShipOnHigh: ask a person to confirm before Ship or Push to
 *   Branch runs on a High-risk change. Default false.
 */
interface Settings {
	bin: string
	mode: Mode
	base?: string
	confirmShipOnHigh: boolean
}

async function settings(amp: PluginAPI): Promise<Settings> {
	const c = await amp.configuration.get()
	const str = (k: string) => (typeof c[k] === 'string' && c[k] ? (c[k] as string) : undefined)
	const backend = str('brisk.backend') ?? 'auto'
	const mode: Mode =
		backend === 'rules-only'
			? 'rules-only'
			: backend === 'openrouter'
				? 'binary'
				: backend === 'amp'
					? 'amp'
					: process.env.OPENROUTER_API_KEY
						? 'binary'
						: 'amp'
	return {
		bin: str('brisk.binary') ?? 'brisk',
		mode,
		base: str('brisk.base'),
		confirmShipOnHigh: c['brisk.confirmShipOnHigh'] === true,
	}
}

interface ThreadState {
	/** Latest result; undefined when the changes may have moved on since. */
	result?: RunResult
	action?: QuickAction
	/** The run in progress. */
	running?: Promise<RunResult | undefined>
	/** A run queued behind `running`; it will see every change made so far. */
	pending?: Promise<RunResult | undefined>
}

const MAX_PAGES = 10 // 20 messages each

/**
 * threadMessages reads the newest MAX_PAGES pages of the transcript plus its
 * first page (for the intent), oldest first. It prefers the full transcript;
 * some threads return nothing with `full: true`, so it then reads what the
 * next turn would see.
 */
async function threadMessages(thread: PluginThread): Promise<ThreadMessage[]> {
	for (const full of [true, false]) {
		const pages: ThreadMessage[][] = []
		let count = 0
		let complete = false
		for (let page = 0; page < MAX_PAGES; page++) {
			const batch = await thread.messages({ full, from: 'end', offset: count, limit: 20 })
			pages.unshift(batch)
			count += batch.length
			if (batch.length < 20) {
				complete = true
				break
			}
		}
		let out = pages.flat()
		if (!complete) {
			const seen = new Set(out.map((m) => m.id))
			const first = await thread.messages({ full, from: 'start', limit: 20 })
			out = [...first.filter((m) => !seen.has(m.id)), ...out]
		}
		if (out.length) return out
	}
	return []
}

export default function (amp: PluginAPI) {
	const threads = new Map<string, ThreadState>()
	const state = (id: string) => {
		let s = threads.get(id)
		if (!s) threads.set(id, (s = {}))
		return s
	}
	const root = amp.system.workspaceRoot ? amp.helpers.filePathFromURI(amp.system.workspaceRoot) : null

	async function run(
		thread: PluginThread,
		decide: Decide,
		log: (...a: unknown[]) => void,
	): Promise<RunResult | undefined> {
		if (!root) return undefined
		const s = state(thread.id)
		// Every caller gets a run that started after it asked, so no result
		// predates the changes it is about. Callers that arrive while a run
		// is queued share that queued run.
		if (s.pending) return s.pending
		const p: Promise<RunResult | undefined> = (s.running ?? Promise.resolve()).then(async () => {
			s.pending = undefined
			s.running = p
			try {
				const cfg = await settings(amp)
				const messages = await threadMessages(thread)
				const session = buildSession(
					thread.id,
					turnsFromMessages(messages, amp.helpers, root),
					await thread.title.get(),
				)
				const result = await assess({ bin: cfg.bin, cwd: root, mode: cfg.mode, base: cfg.base, session, decide })
				s.result = result
				await show(thread, s, result)
				log(`brisk: ${summary(result.assessment, result.degraded)}`)
				return result
			} catch (e) {
				s.result = undefined
				log(`brisk: assessment failed: ${e instanceof Error ? e.message : String(e)}`)
				return undefined
			} finally {
				if (s.running === p) s.running = undefined
			}
		})
		s.pending = p
		return p
	}

	async function show(thread: PluginThread, s: ThreadState, r: RunResult) {
		const a = r.assessment
		await thread.setLabels(labelsFor(await thread.labels(), a.tier))
		if (a.tier === 'low' || a.reasons.length === 0) {
			s.action?.unsubscribe()
			s.action = undefined
			return
		}
		const def = {
			text: a.tier === 'high' ? 'Review high-risk changes' : 'Review risky changes',
			icon: 'eye' as const,
			action: 'send-message' as const,
			args: { message: reviewMessage(a) },
		}
		if (s.action) s.action.update(def)
		else s.action = thread.createQuickAction(def)
	}

	amp.on('agent.end', async (event, ctx) => {
		const s = state(event.thread.id)
		const touched = amp.helpers
			.toolCallsInMessages(event.messages)
			.some(({ call }) => amp.helpers.filesModifiedByToolCall(call)?.length || amp.helpers.shellCommandFromToolCall(call))
		if (event.status !== 'done') {
			// The turn may have changed files; reassess before the next Ship.
			if (touched) s.result = undefined
			return
		}
		if (!touched && s.result) return
		await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log)
	})

	amp.on('changes.prompt', async (event, ctx) => {
		const s = state(event.thread.id)
		const inFlight = s.pending ?? s.running
		const r = inFlight
			? await inFlight
			: (s.result ?? (await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log)))
		const line = r && shipLine(r.assessment, r.degraded)
		return line ? { append: line } : undefined
	})

	amp.on('agent.start', async (event, ctx) => {
		if (!event.changesWorkflow) return {}
		const cfg = await settings(amp)
		if (!cfg.confirmShipOnHigh) return {}
		const r = await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log)
		if (r?.assessment.tier !== 'high') return {}
		const ok = await ctx.ui.confirm({
			title: 'Ship a High-risk change?',
			message: `${summary(r.assessment, r.degraded)}\n\nThis project asks a person to confirm before shipping High-risk changes.`,
			confirmButtonText: 'Ship anyway',
			requireHuman: true,
		})
		if (!ok) await ctx.thread.cancel()
		return {}
	})

	amp.registerCommand(
		'assess',
		{ title: 'Assess change risk', category: 'brisk', description: "Rate the risk of this thread's changes now." },
		async (ctx) => {
			if (!ctx.thread) {
				await ctx.ui.notify('brisk: open a thread first.')
				return
			}
			const thread = ctx.thread
			const r = await run(thread, (req) => ctx.ai.decide({ ...(req as Parameters<typeof ctx.ai.decide>[0]), threadID: thread.id }), amp.logger.log)
			await ctx.ui.notify(r ? summary(r.assessment, r.degraded) : 'brisk: assessment failed; see the plugin log.')
		},
	)

	amp.onDispose(() => {
		for (const s of threads.values()) s.action?.unsubscribe()
	})
}
