// Text and labels the plugin shows for an assessment. Pure functions.

import type { Assessment, Evidence, Tier } from './brisk.ts'

export const LABEL_PREFIX = 'risk-'

const TITLE: Record<Tier, string> = { low: 'Low', medium: 'Medium', high: 'High' }

/** labelsFor replaces any earlier risk label with the one for tier. */
export function labelsFor(existing: string[], tier: Tier): string[] {
	return [...existing.filter((l) => !l.startsWith(LABEL_PREFIX)), LABEL_PREFIX + tier]
}

function where(e: Evidence): string {
	if (!e.start_line) return e.path
	if (!e.end_line || e.end_line === e.start_line) return `${e.path}:${e.start_line}`
	return `${e.path}:${e.start_line}-${e.end_line}`
}

function reasonLines(a: Assessment): string[] {
	return a.reasons.map((r) => {
		const ev = (r.evidence ?? []).slice(0, 3).map(where)
		return ev.length ? `${r.text} (${ev.join(', ')})` : r.text
	})
}

function rulesOnly(a: Assessment, degraded?: string): boolean {
	return Boolean(degraded || a.provenance.degraded || a.provenance.backend === 'rules-only')
}

/** summary is one line for notifications and the Ship prompt. */
export function summary(a: Assessment, degraded?: string): string {
	const reasons = a.reasons.map((r) => r.text.replace(/\.$/, '')).join('; ')
	const mode = rulesOnly(a, degraded) ? ', rules only' : ''
	return `Risk: ${TITLE[a.tier]}${mode}${reasons ? ` (${reasons})` : ''}.`
}

/**
 * shipLine is appended to the Ship and Push to Branch prompt for Medium and
 * High, or undefined for Low.
 */
export function shipLine(a: Assessment, degraded?: string): string | undefined {
	if (a.tier === 'low') return undefined
	const ask =
		a.tier === 'high'
			? 'Before pushing, review the changes behind these reasons, fix anything wrong, and tell me what you checked.'
			: 'Before pushing, check the changes behind these reasons.'
	return `${summary(a, degraded)} ${ask}`
}

/** reviewMessage is what the "Review risky changes" quick action sends. */
export function reviewMessage(a: Assessment): string {
	const lines = reasonLines(a).map((l) => `- ${l}`)
	return [
		`brisk rated these changes ${TITLE[a.tier]} risk. Review them before shipping:`,
		...lines,
		'',
		'For each reason, read the code it points to and check whether the change is correct and covered by tests. Fix real problems, and tell me which reasons were false alarms.',
	].join('\n')
}
