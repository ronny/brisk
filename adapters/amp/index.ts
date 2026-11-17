// brisk for Amp: every change gets one agent review, guided by brisk's plan;
// brisk decides whether a person should look too. The plugin labels the
// thread (brisk-risk-<tier>, plus brisk-needs-human), runs the review before Ship, and asks
// a person the questions brisk lists in a dialog before Ship. Settings come
// only from user and managed settings files; see src/settings.ts.

import type { PluginAPI, PluginThread, QuickAction } from '@ampcode/plugin'
import { publishAssessment, type Tools } from './src/artifact.ts'
import { assess, type Decide, type RunResult } from './src/brisk.ts'
import {
	agentReviewMessage,
	humanDialog,
	humanLine,
	labelsFor,
	needsHuman,
	REVIEW_PREFIX,
	reviewMessage,
	reviewWanted,
	shipLine,
	summary,
	unassessedGate,
	unattendedNote,
	unknownLine,
} from './src/present.ts'
import { buildSession, readTranscript, turnsFromMessages } from './src/session.ts'
import { readTrusted, resolveBinary, reviewMode, settingsFrom, type AskPerson, type ReviewMode } from './src/settings.ts'

export const description =
	"Runs one agent review of the thread's code changes before Ship, guided by brisk, and asks a person to look too when brisk says so, with the questions to think about. Labels the thread brisk-risk-low, -medium or -high, plus brisk-needs-human, and keeps a Brisk assessment artifact in the thread."

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
		tools?: Tools,
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
				await show(thread, s, result, reviewMode(cfg, result.assessment.provenance.preset), cfg.askPerson).catch((e) => log(`brisk: could not show the result: ${e instanceof Error ? e.message : String(e)}`))
				// The binary's version, so a result can be traced to the release that made it.
				log(`brisk ${result.assessment.provenance.brisk_version}: ${summary(result.assessment, result.degraded)}`)
				if (tools && cfg.artifact && s.epoch === epoch)
					await publishAssessment(tools, result.assessment, result.degraded, new Date()).catch((e) =>
						log(`brisk: could not publish the assessment artifact: ${e instanceof Error ? e.message : String(e)}`),
					)
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

	async function show(thread: PluginThread, s: ThreadState, r: RunResult, mode: ReviewMode, ask: AskPerson) {
		const a = r.assessment
		if (mode !== 'off' && reviewWanted(a)) {
			const def = { text: 'Start agent review', icon: 'sparkles' as const, action: 'send-message' as const, args: { message: agentReviewMessage(a) } }
			if (s.reviewAction) s.reviewAction.update(def)
			else s.reviewAction = thread.createQuickAction(def)
			s.reviewOffered = a.provenance.diff_hash
		} else {
			s.reviewAction?.unsubscribe()
			s.reviewAction = undefined
			s.reviewOffered = undefined
		}
		await thread.setLabels(labelsFor(await thread.labels(), a.tier, ask !== 'off' && needsHuman(a)))
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
		const r = await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log, ctx.tools)
		if (!r || 'error' in r || !reviewWanted(r.assessment)) return
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
			: (s.result ?? (await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log, ctx.tools)))
		if (!r) return undefined
		if ('error' in r) return { append: unknownLine(r.error) }
		const cfg = await settings()
		const lines = [shipLine(r.assessment, r.degraded), cfg.askPerson === 'off' ? undefined : humanLine(r.assessment)].filter(Boolean)
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
		// assessment unless the user turned everything off.
		if (!cfg.confirmShip && cfg.agentReview === 'off' && cfg.askPerson === 'off') return {}
		// Always a fresh assessment: files may have changed outside a turn,
		// and whether a review already covers them depends on the diff hash.
		const r = await run(ctx.thread, (req) => ctx.ai.decide(req as Parameters<typeof ctx.ai.decide>[0]), ctx.logger.log, ctx.tools)
		// before-ship (or after-turn, when the review has not run yet for
		// this version): the Ship turn runs the review before pushing.
		let review: { message: { content: string; display: boolean } } | {} = {}
		let reviewing: string | undefined
		if (r && !('error' in r) && reviewWanted(r.assessment)) {
			const mode = reviewMode(cfg, r.assessment.provenance.preset)
			const preset = r.assessment.provenance.preset ?? ''
			// before-ship reviews on every Ship; a careful repository asks for
			// that even when the user chose after-turn, which ranks above it.
			const always = mode === 'before-ship' || (mode !== 'off' && reviewMode({ ...cfg, agentReview: 'suggest' }, preset) === 'before-ship')
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
		const withReview = (note?: string) => {
			reviewed()
			if (!note) return review
			const content = 'message' in review ? `${review.message.content}\n\n${note}` : note
			return { message: { content, display: true } }
		}
		if (!r) return withReview()
		if ('error' in r) {
			const gate = unassessedGate(cfg.confirmShip, r)
			if (gate && !(await ctx.ui.confirm({ ...gate, confirmButtonText: 'Ship anyway', requireHuman: true }))) {
				await ctx.thread.cancel()
				return {}
			}
			return withReview()
		}
		if (!needsHuman(r.assessment) || cfg.askPerson !== 'dialog') return withReview()
		// A person answers; when nobody watches the thread the dialog is
		// skipped and the Ship turn stops before pushing (or ships, if the
		// user chose that) and leaves the questions in its reply.
		const ok = await ctx.ui.confirm({
			...humanDialog(r.assessment, r.degraded),
			confirmButtonText: "I've thought about it, ship",
			requireHuman: true,
			skipIfUnwatched: true,
		})
		if (ok === false) {
			await ctx.thread.cancel()
			return {}
		}
		if (ok === undefined) return withReview(unattendedNote(r.assessment, cfg.unattended === 'ship'))
		return withReview()
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
			const r = await run(thread, (req) => ctx.ai.decide({ ...(req as Parameters<typeof ctx.ai.decide>[0]), threadID: thread.id }), amp.logger.log, ctx.tools)
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
