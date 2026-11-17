// brisk for Amp: rates the risk of a thread's code changes with the brisk
// binary and shows it as a thread label, a quick action, and a line in the
// Ship prompt. Advisory unless the user turns on brisk.confirmShipOnHigh for
// a project. Settings come only from user and managed settings files; see
// src/settings.ts.

import type { PluginAPI, PluginThread, QuickAction } from '@ampcode/plugin'
import { assess, type Decide, type RunResult } from './src/brisk.ts'
import { agentReviewMessage, labelsFor, reviewLine, reviewMessage, reviewRecommended, shipGate, shipLine, summary, unknownLine } from './src/present.ts'
import { buildSession, readTranscript, turnsFromMessages } from './src/session.ts'
import { PROJECT_RAISE, readTrusted, resolveBinary, reviewMode, settingsFrom, type ReviewMode } from './src/settings.ts'

export const description =
	"Rates how risky the thread's code changes are (Low, Medium, High) with brisk, and whether an agent review is recommended. Labels the thread, adds review quick actions, notes the risk in the Ship prompt, and can run the agent review automatically."

/** Every agent review message starts with this; see agentReviewMessage. */
const REVIEW_PREFIX = 'brisk recommends an agent review'

/** A run's result, or why it failed. */
type Outcome = RunResult | { error: string }

interface ThreadState {
	/** Latest result; undefined when the changes may have moved on since. */
	result?: RunResult
	action?: QuickAction
	/** The "Start agent review" quick action. */
	reviewAction?: QuickAction
	/** Diff hash of the last automatic review the plugin asked for. */
	reviewRequested?: string
	/** Diff hash the Start agent review quick action is for. */
	reviewOffered?: string
	/** Diff hash the current turn reviews, and that turn; it counts once the turn ends done. */
	reviewStarted?: string
	reviewTurn?: number
	/** Diff hash of the last review that actually started, so each version is reviewed once. */
	autoReviewed?: string
	/** Counts agent turns, so a slow agent.end does not continue after the user moved on. */
	turn: number
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
		if (!s) threads.set(id, (s = { epoch: 0, turn: 0 }))
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
				await show(thread, s, result, reviewMode(cfg, result.assessment.provenance.preset)).catch((e) => log(`brisk: could not show the result: ${e instanceof Error ? e.message : String(e)}`))
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
				s.reviewAction?.unsubscribe()
				s.reviewAction = undefined
				s.reviewOffered = undefined
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

	async function show(thread: PluginThread, s: ThreadState, r: RunResult, mode: ReviewMode) {
		const a = r.assessment
		if (mode !== 'off' && reviewRecommended(a)) {
			const def = { text: 'Start agent review', icon: 'sparkles' as const, action: 'send-message' as const, args: { message: agentReviewMessage(a) } }
			if (s.reviewAction) s.reviewAction.update(def)
			else s.reviewAction = thread.createQuickAction(def)
			s.reviewOffered = a.provenance.diff_hash
		} else {
			s.reviewAction?.unsubscribe()
			s.reviewAction = undefined
			s.reviewOffered = undefined
		}
		await thread.setLabels(labelsFor(await thread.labels(), a.tier))
		// Amp shows 5 quick actions per thread across plugins; the agent review
		// covers what "Review risky changes" would ask.
		if (a.tier === 'low' || a.reasons.length === 0 || s.reviewAction) {
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
		// A review counts only when its turn finished.
		if (s.reviewTurn === s.turn) {
			if (event.status === 'done' && s.reviewStarted) s.autoReviewed = s.reviewStarted
			s.reviewStarted = s.reviewTurn = undefined
		}
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
		const turn = s.turn
		const epoch = s.epoch
		const r = await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log)
		if (!r || 'error' in r || !reviewRecommended(r.assessment)) return
		// after-turn: start the review now, once per version of the changes,
		// and never right after a review turn, so reviews cannot loop.
		const a = r.assessment
		if (reviewMode(await settings(), a.provenance.preset) !== 'after-turn') return
		if (s.autoReviewed === a.provenance.diff_hash || event.message.startsWith(REVIEW_PREFIX)) return
		if (s.turn !== turn || s.epoch !== epoch) return
		// Marked done when the review turn starts (agent.start), in case Amp
		// does not run this continuation.
		s.reviewRequested = a.provenance.diff_hash
		return { action: 'continue' as const, userMessage: agentReviewMessage(a, { unattended: true }), maxContinuations: 1 }
	})

	amp.on('changes.prompt', async (event, ctx) => {
		const s = state(event.thread.id)
		const inFlight = s.pending ?? s.running
		const r = inFlight
			? await inFlight
			: (s.result ?? (await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log)))
		if (!r) return undefined
		if ('error' in r) return { append: unknownLine(r.error) }
		const mode = reviewMode(await settings(), r.assessment.provenance.preset)
		const lines = [shipLine(r.assessment, r.degraded), mode === 'off' ? undefined : reviewLine(r.assessment)].filter(Boolean)
		return lines.length ? { append: lines.join(' ') } : undefined
	})

	amp.on('agent.start', async (event, ctx) => {
		const s = state(event.thread.id)
		s.turn++
		// A review turn, started by the quick action or after-turn, counts
		// as the review of the version it was offered for.
		if (event.message.startsWith(REVIEW_PREFIX) && (s.reviewOffered ?? s.reviewRequested)) {
			s.reviewStarted = s.reviewOffered ?? s.reviewRequested
			s.reviewTurn = s.turn
		}
		if (!event.changesWorkflow) return {}
		const cfg = await settings()
		// The project's preset can only turn before-ship on, so look at the
		// assessment unless the user turned agent review off entirely.
		if (!cfg.confirmShip && cfg.agentReview === 'off') return {}
		// Always a fresh assessment: files may have changed outside a turn,
		// and whether a review already covers them depends on the diff hash.
		const r = await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log)
		// before-ship (or after-turn, when the review has not run yet for
		// this version): the Ship turn runs the review before pushing.
		let review: { message: { content: string; display: boolean } } | {} = {}
		let reviewing: string | undefined
		if (r && !('error' in r) && reviewRecommended(r.assessment)) {
			const mode = reviewMode(cfg, r.assessment.provenance.preset)
			const preset = r.assessment.provenance.preset ?? ''
			// before-ship reviews on every Ship; a strict repository asks for
			// that even when the user chose after-turn, which ranks above it.
			const always = mode === 'before-ship' || (mode !== 'off' && Object.hasOwn(PROJECT_RAISE, preset) && PROJECT_RAISE[preset] === 'before-ship')
			if (always || (mode !== 'suggest' && mode !== 'off' && s.autoReviewed !== r.assessment.provenance.diff_hash)) {
				reviewing = r.assessment.provenance.diff_hash
				review = { message: { content: `Before pushing: ${agentReviewMessage(r.assessment)} Stop before pushing if it finds a blocker you cannot fix.`, display: true } }
			}
		}
		// This Ship turn runs the review, so its agent.end must not start another.
		const reviewed = () => {
			if (!reviewing) return
			s.reviewStarted = reviewing
			s.reviewTurn = s.turn
		}
		const ask = cfg.confirmShip && r ? shipGate(true, r) : undefined
		if (!ask) {
			reviewed()
			return review
		}
		const ok = await ctx.ui.confirm({
			...ask,
			confirmButtonText: 'Ship anyway',
			requireHuman: true,
		})
		if (!ok) {
			await ctx.thread.cancel()
			return {}
		}
		reviewed()
		return review
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
		for (const s of threads.values()) {
			s.action?.unsubscribe()
			s.reviewAction?.unsubscribe()
		}
	})
}
