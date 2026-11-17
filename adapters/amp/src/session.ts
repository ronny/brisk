// Builds brisk's harness-neutral session record (schema/session.schema.json)
// from Amp thread messages. Pure functions, so they test without Amp.

import type { PluginAPI, PluginThread, ThreadMessage } from '@ampcode/plugin'

export interface Command {
	command: string
	exit_code?: number
}

export interface Excerpt {
	role: 'user' | 'assistant'
	text: string
}

export interface Session {
	schema: 'brisk.session/v1'
	harness: 'amp'
	session_id: string
	intent?: string
	files_touched?: string[]
	commands?: Command[]
	excerpts?: Excerpt[]
}

/** The helpers Amp passes plugins; injected so tests can supply fakes. */
export type Helpers = Pick<
	PluginAPI['helpers'],
	'toolCallsInMessages' | 'shellCommandFromToolCall' | 'filesModifiedByToolCall' | 'filePathFromURI'
>

const MAX_INTENT = 2000
const MAX_EXCERPT = 1500
const MAX_EXCERPTS_TOTAL = 8000
const MAX_COMMANDS = 200

/** One agent turn's contribution to the session. */
export interface Turn {
	userMessage: string
	files: string[]
	commands: Command[]
	finalText?: string
}

/** exitCode reads a shell tool's exit code from its result, if it has one. */
export function exitCode(output: unknown): number | undefined {
	if (output && typeof output === 'object' && !Array.isArray(output)) {
		const o = output as Record<string, unknown>
		for (const k of ['exitCode', 'exit_code', 'exitStatus']) {
			if (typeof o[k] === 'number') return o[k] as number
		}
		if (typeof o.output === 'string') return exitCode(o.output)
		return undefined
	}
	let text = ''
	if (typeof output === 'string' && output.trimStart().startsWith('{')) {
		// Amp's shell tool result: '{"output":"...","exitCode":0}'
		try {
			return exitCode(JSON.parse(output))
		} catch {
			// not JSON; fall through to the text patterns
		}
	}
	if (typeof output === 'string') text = output
	else if (Array.isArray(output)) {
		text = output.map((b) => (b && typeof b === 'object' && 'text' in b ? String(b.text) : '')).join('\n')
	}
	const m = /<exitCode>\s*(-?\d+)\s*<\/exitCode>|\bexit(?:ed with)? (?:code|status):?\s*(-?\d+)/i.exec(text)
	if (m) return Number(m[1] ?? m[2])
	return undefined
}

/** relative turns an absolute path into a path relative to root, or keeps it. */
export function relative(path: string, root: string | null): string {
	if (!root) return path
	const r = root.endsWith('/') ? root : root + '/'
	return path.startsWith(r) ? path.slice(r.length) : path
}

function lastAssistantText(messages: ThreadMessage[]): string | undefined {
	for (let i = messages.length - 1; i >= 0; i--) {
		const m = messages[i]
		if (m.role !== 'assistant') continue
		const text = m.content
			.filter((b) => b.type === 'text')
			.map((b) => (b as { text: string }).text)
			.join('\n')
			.trim()
		if (text) return text
	}
	return undefined
}

/** turnFromMessages extracts what one agent turn did. */
export function turnFromMessages(
	userMessage: string,
	messages: ThreadMessage[],
	helpers: Helpers,
	root: string | null,
): Turn {
	const files = new Set<string>()
	const commands: Command[] = []
	for (const { call, result } of helpers.toolCallsInMessages(messages)) {
		if (result.status !== 'done' && result.status !== 'error') continue
		for (const uri of helpers.filesModifiedByToolCall(call) ?? []) {
			files.add(relative(helpers.filePathFromURI(uri), root))
		}
		const shell = helpers.shellCommandFromToolCall(call)
		if (shell) {
			const code = exitCode(result.output) ?? (result.status === 'error' ? 1 : undefined)
			commands.push(code === undefined ? { command: shell.command } : { command: shell.command, exit_code: code })
		}
	}
	return { userMessage, files: [...files], commands, finalText: lastAssistantText(messages) }
}

function userText(m: ThreadMessage): string | undefined {
	if (m.role !== 'user') return undefined
	const text = m.content
		.filter((b) => b.type === 'text')
		.map((b) => (b as { text: string }).text)
		.join('\n')
		.trim()
	return text || undefined
}

/**
 * turnsFromMessages splits a transcript, oldest first, into turns. A turn
 * starts at each user message with text; tool results are user messages too,
 * but carry no text.
 */
export function turnsFromMessages(messages: ThreadMessage[], helpers: Helpers, root: string | null): Turn[] {
	const turns: Turn[] = []
	let start = -1
	const flush = (end: number) => {
		if (start >= 0) turns.push(turnFromMessages(userText(messages[start])!, messages.slice(start, end), helpers, root))
	}
	messages.forEach((m, i) => {
		if (userText(m) !== undefined) {
			flush(i)
			start = i
		}
	})
	flush(messages.length)
	return turns
}

function clip(s: string, n: number): string {
	s = s.trim()
	return s.length <= n ? s : s.slice(0, n - 1) + '…'
}

/**
 * buildSession folds the turns of a thread, oldest first, into one session
 * record. The first user message is the intent; later user messages are kept
 * as excerpts because they hold corrections. The excerpts keep the newest
 * text when they exceed the size budget.
 */
export function buildSession(threadID: string, turns: Turn[], title?: string | null): Session {
	const s: Session = { schema: 'brisk.session/v1', harness: 'amp', session_id: threadID }
	if (turns.length === 0) return s
	const intent = [title, turns[0].userMessage].filter(Boolean).join('\n\n')
	if (intent) s.intent = clip(intent, MAX_INTENT)

	const files = new Set<string>()
	const commands: Command[] = []
	const excerpts: Excerpt[] = []
	turns.forEach((t, i) => {
		t.files.forEach((f) => files.add(f))
		commands.push(...t.commands)
		if (i > 0 && t.userMessage.trim()) excerpts.push({ role: 'user', text: clip(t.userMessage, MAX_EXCERPT) })
		if (t.finalText) excerpts.push({ role: 'assistant', text: clip(t.finalText, MAX_EXCERPT) })
	})
	// Keep only the latest assistant summary, plus every user correction.
	const lastAssistant = excerpts.findLastIndex((e) => e.role === 'assistant')
	let kept = excerpts.filter((e, i) => e.role === 'user' || i === lastAssistant)
	while (kept.reduce((n, e) => n + e.text.length, 0) > MAX_EXCERPTS_TOTAL && kept.length > 1) {
		kept = kept.slice(1)
	}
	if (files.size) s.files_touched = [...files].sort()
	if (commands.length) s.commands = commands.slice(-MAX_COMMANDS)
	if (kept.length) s.excerpts = kept
	return s
}

const MAX_PAGES = 10 // 20 messages each, overlapping by one

/**
 * readTranscript reads the transcript, oldest first: all of it when it is
 * short, else its first page (for the intent) and about the newest 190
 * messages. It prefers the full transcript; some threads return nothing with
 * `full: true`, so it then reads what the next turn would see.
 *
 * Each call is a round trip (~400 ms), so after the newest page it fetches
 * the remaining pages at once. They overlap by one message, so a message
 * appended during the read cannot fall between two pages.
 */
export async function readTranscript(thread: Pick<PluginThread, 'messages'>): Promise<ThreadMessage[]> {
	for (const full of [true, false]) {
		const newest = await thread.messages({ full, from: 'end', limit: 20 })
		if (newest.length === 0) continue
		if (newest.length < 20) return newest
		const [first, ...older] = await Promise.all([
			thread.messages({ full, from: 'start', limit: 20 }),
			...Array.from({ length: MAX_PAGES - 1 }, (_, i) =>
				thread.messages({ full, from: 'end', offset: (i + 1) * 19, limit: 20 }),
			),
		])
		// older is newest first; put everything oldest first and drop repeats.
		const seen = new Set<ThreadMessage['id']>()
		const out: ThreadMessage[] = []
		for (const m of [...first, ...older.reverse().flat(), ...newest]) {
			if (seen.has(m.id)) continue
			seen.add(m.id)
			out.push(m)
		}
		return out
	}
	return []
}
