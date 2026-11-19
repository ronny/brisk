// Publishes the thread's "Brisk assessment" artifact through Amp's artifact
// tool: one artifact per thread, updated after every assessment.

import type { Assessment } from './brisk.ts'
import { ARTIFACT_TITLE, assessmentMarkdown, historyFrom, historyLine } from './present.ts'

/** The part of ctx.tools the artifact needs. */
export interface Tools {
	call(name: string, input: Record<string, unknown>): Promise<unknown>
}

interface ArtifactInfo {
	artifact_id: string
	title: string
	revision: number
	url?: string
	content?: string
}

/**
 * publishAssessment creates the thread's artifact, or updates the one with
 * ARTIFACT_TITLE, keeping its history list. It resolves with the URL.
 */
export async function publishAssessment(tools: Tools, a: Assessment, degraded: string | undefined, now: Date): Promise<string | undefined> {
	const list = (await tools.call('artifact', { action: 'list' })) as { artifacts?: ArtifactInfo[] }
	const found = (list.artifacts ?? []).find((x) => x.title === ARTIFACT_TITLE)
	if (!found) {
		const out = (await tools.call('artifact', {
			action: 'publish',
			title: ARTIFACT_TITLE,
			kind: 'document',
			content: assessmentMarkdown(a, [historyLine(a, now)], degraded),
		})) as ArtifactInfo
		return out.url
	}
	const cur = (await tools.call('artifact', { action: 'read', artifact_id: found.artifact_id })) as ArtifactInfo
	const content = assessmentMarkdown(a, [historyLine(a, now), ...historyFrom(cur.content ?? '')], degraded)
	const out = (await tools.call('artifact', {
		action: 'publish',
		artifact_id: found.artifact_id,
		base_revision: cur.revision,
		content,
	})) as ArtifactInfo
	return out.url ?? found.url
}
