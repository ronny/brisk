// brisk for Amp: rates the risk of a thread's code changes with the brisk
// binary and shows it as a thread label, a quick action, and a line in the
// Ship prompt. Advisory unless the user turns on brisk.confirmShipOnHigh for
// a project. Settings come only from user and managed settings files; see
// src/settings.ts.

import type { PluginAPI, PluginThread, QuickAction } from '@ampcode/plugin'
import { assess, type Decide, type RunResult } from './src/brisk.ts'
import { labelsFor, reviewMessage, shipGate, shipLine, summary, unknownLine } from './src/present.ts'
import { buildSession, readTranscript, turnsFromMessages } from './src/session.ts'
import { readTrusted, resolveBinary, settingsFrom } from './src/settings.ts'

export const description =
	"Rates how risky the thread's code changes are (Low, Medium, High) with brisk. Labels the thread, adds a review quick action, and notes Medium and High risk in the Ship prompt."

/** A run's result, or why it failed. */
type Outcome = RunResult | { error: string }

interface ThreadState {
	/** Latest result; undefined when the changes may have moved on since. */
	result?: RunResult
	action?: QuickAction
	/** The run in progress. */
	running?: Promise<Outcome | undefined>
	/** A run queued behind `running`; it will see every change made so far. */
	pending?: Promise<Outcome | undefined>
	/** Bumped when files may have changed outside a run, so an older run does not save its result. */
	epoch: number
}

export default function (amp: PluginAPI) {
	const threads = new Map<string, ThreadState>()
	const state = (id: string) => {
		let s = threads.get(id)
		if (!s) threads.set(id, (s = { epoch: 0 }))
		return s
	}
	const root = amp.system.workspaceRoot ? amp.helpers.filePathFromURI(amp.system.workspaceRoot) : null
	const settings = async () => settingsFrom(await readTrusted(), root ?? '')

	async function run(
		thread: PluginThread,
		decide: Decide,
		log: (...a: unknown[]) => void,
	): Promise<Outcome | undefined> {
		if (!root) return undefined
		const s = state(thread.id)
		// Every caller gets a run that started after it asked, so no result
		// predates the changes it is about. Callers that arrive while a run
		// is queued share that queued run.
		if (s.pending) return s.pending
		const p: Promise<Outcome | undefined> = (s.running ?? Promise.resolve()).catch(() => {}).then(async () => {
			s.pending = undefined
			s.running = p
			const epoch = s.epoch
			try {
				const cfg = await settings()
				const messages = await readTranscript(thread)
				const session = buildSession(
					thread.id,
					turnsFromMessages(messages, amp.helpers, root),
					await thread.title.get(),
				)
				const bin = await resolveBinary(cfg.bin, root)
				const result = await assess({ bin, cwd: root, mode: cfg.mode, base: cfg.base, session, decide })
				if (s.epoch === epoch) s.result = result
				await show(thread, s, result).catch((e) => log(`brisk: could not show the result: ${e instanceof Error ? e.message : String(e)}`))
				log(`brisk: ${summary(result.assessment, result.degraded)}`)
				return result
			} catch (e) {
				const error = e instanceof Error ? e.message : String(e)
				s.result = undefined
				log(`brisk: assessment failed: ${error}`)
				// An earlier label and review action describe changes that have
				// moved on; replace them rather than leave a stale Low.
				s.action?.unsubscribe()
				s.action = undefined
				await thread
					.labels()
					.then((l) => thread.setLabels(labelsFor(l, 'unknown')))
					.catch(() => {})
				return { error }
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
			if (touched) {
				s.result = undefined
				s.epoch++
			}
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
		if (!r) return undefined
		const line = 'error' in r ? unknownLine(r.error) : shipLine(r.assessment, r.degraded)
		return line ? { append: line } : undefined
	})

	amp.on('agent.start', async (event, ctx) => {
		if (!event.changesWorkflow) return {}
		const cfg = await settings()
		if (!cfg.confirmShip) return {}
		const r = await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log)
		const ask = r && shipGate(true, r)
		if (!ask) return {}
		const ok = await ctx.ui.confirm({
			...ask,
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
			await ctx.ui.notify(!r ? 'brisk: no workspace to assess.' : 'error' in r ? `brisk: assessment failed: ${r.error}` : summary(r.assessment, r.degraded))
		},
	)

	amp.onDispose(() => {
		for (const s of threads.values()) s.action?.unsubscribe()
	})
}
