import { afterAll, beforeAll, describe, expect, test } from 'bun:test'
import { chmod, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { ThreadMessage } from '@ampcode/plugin'
import { assess, type Assessment } from '../src/brisk.ts'
import { labelsFor, shipLine, summary } from '../src/present.ts'
import { buildSession, exitCode, readTranscript, turnsFromMessages, type Helpers } from '../src/session.ts'

const floor: Assessment = {
	schema: 'brisk.assessment/v1',
	tier: 'medium',
	floor: 'medium',
	impact: 2,
	uncertainty: 1,
	action: 'Review the listed files.',
	reasons: [{ text: 'Changes auth code.', source: 'rule', id: 'auth', tier: 'medium', evidence: [{ path: 'auth.go', start_line: 4, end_line: 6 }] }],
	stats: { files: 1, additions: 2, deletions: 1 },
	provenance: { brisk_version: 'test', backend: 'rules-only', diff_hash: 'h', rule_hits: ['auth'], duration_ms: 1 },
}

// A fake brisk that follows the host protocol in README.md and logs its arguments.
let dir: string
let bin: string
beforeAll(async () => {
	dir = await mkdtemp(join(tmpdir(), 'brisk-fake-'))
	bin = join(dir, 'brisk')
	await writeFile(join(dir, 'floor.json'), JSON.stringify(floor))
	await writeFile(
		bin,
		`#!/usr/bin/env bash
echo "$*" >> "${dir}/args.log"
for a in "$@"; do case "$prev" in --answers) ans="$a";; --session) cp "$a" "${dir}/session.json";; --backend) backend="$a";; esac; prev="$a"; done
[ -n "$FAKE_FAIL" ] && { echo "boom" >&2; exit 1; }
if [ -n "$ans" ]; then
  model=$(sed -n 's/.*"model":"\\([^"]*\\)".*/\\1/p' "$ans")
  sed "s/\\"tier\\":\\"medium\\"/\\"tier\\":\\"high\\"/; s/\\"backend\\":\\"rules-only\\"/\\"backend\\":\\"host\\",\\"model\\":\\"$model\\"/" "${dir}/floor.json"
elif [ "$backend" = host ]; then
  printf '{"schema":"brisk.host-request/v1","request_id":"r1","model":"typesafe/jev","state":"S","questions":{"impact":{"type":"score","instructions":"i","criteria":["a","b"]}},"floor_assessment":%s}' "$(cat ${dir}/floor.json)"
else
  cat "${dir}/floor.json"
fi
`,
	)
	await chmod(bin, 0o755)
})
afterAll(() => rm(dir, { recursive: true, force: true }))

describe('assess', () => {
	test('host backend: passes state and questions to decide, sends answers back', async () => {
		let seen: unknown
		const r = await assess({
			bin,
			cwd: dir,
			mode: 'amp',
			session: { schema: 'brisk.session/v1', session_id: 'T-1' },
			decide: async (req) => {
				seen = req
				return { model: 'jev-1.14.0', answers: { impact: { type: 'score', score: 2.5 } } }
			},
		})
		expect(seen).toEqual({
			model: 'typesafe/jev',
			state: 'S',
			questions: { impact: { type: 'score', instructions: 'i', criteria: ['a', 'b'] } },
		})
		expect(r.assessment.tier).toBe('high')
		expect(r.assessment.provenance.model).toBe('jev-1.14.0')
		expect(r.degraded).toBeUndefined()
		const log = await readFile(join(dir, 'args.log'), 'utf8')
		expect(log).toContain('--backend host')
		expect(log).toMatch(/--answers \S+answers\.json/)
		expect(JSON.parse(await readFile(join(dir, 'session.json'), 'utf8')).session_id).toBe('T-1')
	})

	test('decide failure falls back to the rules-only floor and says why', async () => {
		const r = await assess({ bin, cwd: dir, mode: 'amp', decide: async () => { throw new Error('Jev disabled') } })
		expect(r.assessment.tier).toBe('medium')
		expect(r.degraded).toContain('Jev disabled')
		expect(summary(r.assessment, r.degraded)).toContain('rules only')
	})

	test('binary mode runs brisk once with --backend auto', async () => {
		await rm(join(dir, 'args.log'), { force: true })
		const r = await assess({ bin, cwd: dir, mode: 'binary' })
		expect(r.assessment.tier).toBe('medium')
		expect((await readFile(join(dir, 'args.log'), 'utf8')).trim().split('\n')).toEqual(['assess --format json --backend auto'])
	})

	test('brisk errors surface stderr', async () => {
		expect(assess({ bin, cwd: dir, mode: 'binary', env: { ...process.env, FAKE_FAIL: '1' } })).rejects.toThrow('boom')
	})
})

describe('present', () => {
	test('labelsFor replaces only the old risk label', () => {
		expect(labelsFor(['wip', 'risk-low'], 'high')).toEqual(['wip', 'risk-high'])
	})
	test('shipLine is only for Medium and High', () => {
		expect(shipLine({ ...floor, tier: 'low' })).toBeUndefined()
		expect(shipLine(floor)).toBe('Risk: Medium, rules only (Changes auth code). Before pushing, check the changes behind these reasons.')
		expect(shipLine({ ...floor, tier: 'high', provenance: { ...floor.provenance, backend: 'host' } })).toStartWith('Risk: High (Changes auth code). Before pushing, review')
	})
})

describe('session', () => {
	const call = (id: string, name: string, input: Record<string, unknown>) => ({ type: 'tool_use' as const, id, name, input })
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	const res = (id: string, output: any, status: 'done' | 'error' = 'done') => ({ type: 'tool_result' as const, toolUseID: id, output, status })
	const messages: ThreadMessage[] = [
		{ role: 'user', id: 1, content: [{ type: 'text', text: 'Fix the login bug' }] },
		{ role: 'assistant', id: 2, content: [call('a', 'edit_file', { path: '/repo/auth/login.go' }), call('b', 'shell_command', { command: 'go test ./...' })] },
		{ role: 'user', id: 3, content: [res('a', 'ok'), res('b', '<output>FAIL</output>\n<exitCode>1</exitCode>')] },
		{ role: 'assistant', id: 4, content: [{ type: 'text', text: 'Tests fail; fixed half.' }] },
		{ role: 'user', id: 5, content: [{ type: 'text', text: 'No, keep the old token check' }] },
		{ role: 'assistant', id: 6, content: [call('c', 'shell_command', { command: 'go test ./auth' })] },
		{ role: 'user', id: 7, content: [res('c', { output: 'ok', exitCode: 0 })] },
		{ role: 'assistant', id: 8, content: [{ type: 'text', text: 'Done, tests pass.' }] },
	]
	// Minimal stand-ins for amp.helpers with the same contracts.
	const helpers: Helpers = {
		toolCallsInMessages: (ms) => {
			const calls = new Map<string, { tool: string; input: Record<string, unknown>; toolUseID: string }>()
			const out: ReturnType<Helpers['toolCallsInMessages']> = []
			for (const m of ms)
				for (const b of m.content as { type: string }[]) {
					if (b.type === 'tool_use') {
						const u = b as ReturnType<typeof call>
						calls.set(u.id, { tool: u.name, input: u.input, toolUseID: u.id })
					} else if (b.type === 'tool_result') {
						const r = b as ReturnType<typeof res>
						const c = calls.get(r.toolUseID)!
						out.push({ call: c, result: { ...c, status: r.status, output: r.output } })
					}
				}
			return out
		},
		shellCommandFromToolCall: (c) => (c.tool === 'shell_command' ? { command: String(c.input.command) } : null),
		filesModifiedByToolCall: (c) => (c.tool === 'edit_file' ? [{ scheme: 'file', path: String(c.input.path) } as never] : null),
		filePathFromURI: (u) => (u as unknown as { path: string }).path,
	}

	test('builds the session record from the transcript', () => {
		const s = buildSession('T-1', turnsFromMessages(messages, helpers, '/repo'), 'Login bug')
		expect(s).toEqual({
			schema: 'brisk.session/v1',
			harness: 'amp',
			session_id: 'T-1',
			intent: 'Login bug\n\nFix the login bug',
			files_touched: ['auth/login.go'],
			commands: [
				{ command: 'go test ./...', exit_code: 1 },
				{ command: 'go test ./auth', exit_code: 0 },
			],
			excerpts: [
				{ role: 'user', text: 'No, keep the old token check' },
				{ role: 'assistant', text: 'Done, tests pass.' },
			],
		})
	})

	// A thread whose messages() pages like Amp's: clamped to 20, from either end.
	const fakeThread = (n: number, fullEmpty = false) => {
		const all: ThreadMessage[] = Array.from({ length: n }, (_, i) => ({ role: 'user', id: i, content: [{ type: 'text', text: `m${i}` }] }))
		return {
			messages: async (o: { full?: boolean; from?: 'start' | 'end'; offset?: number; limit?: number } = {}) => {
				if (o.full && fullEmpty) return []
				const limit = Math.min(o.limit ?? 10, 20)
				const offset = o.offset ?? 0
				if (o.from === 'start') return all.slice(offset, offset + limit)
				const end = n - offset
				return end <= 0 ? [] : all.slice(Math.max(0, end - limit), end)
			},
		}
	}
	const ids = (ms: ThreadMessage[]) => ms.map((m) => m.id as number)

	const range = (from: number, to: number) => Array.from({ length: to - from }, (_, i) => from + i)

	test('readTranscript returns threads up to 211 messages whole and in order', async () => {
		for (const n of [1, 19, 20, 21, 30, 200, 210, 211]) {
			expect(ids(await readTranscript(fakeThread(n)))).toEqual(range(0, n))
		}
	})

	test('readTranscript keeps the first page and the newest 191 messages of a long thread', async () => {
		for (const n of [212, 221, 500]) {
			expect(ids(await readTranscript(fakeThread(n)))).toEqual([...range(0, 20), ...range(n - 191, n)])
		}
	})

	test('readTranscript reads a short thread in one call', async () => {
		const t = fakeThread(12)
		let calls = 0
		const counted = { messages: (o: Parameters<typeof t.messages>[0]) => (calls++, t.messages(o)) }
		await readTranscript(counted)
		expect(calls).toBe(1)
	})

	test('readTranscript falls back when full: true returns nothing', async () => {
		expect(ids(await readTranscript(fakeThread(3, true)))).toEqual([0, 1, 2])
	})

	test('exitCode reads the shapes shell results come in', () => {
		expect(exitCode('<exitCode>2</exitCode>')).toBe(2)
		expect(exitCode({ exitCode: 0 })).toBe(0)
		expect(exitCode('{"output":"FAIL\\n","exitCode":1}')).toBe(1)
		expect(exitCode('{not json} exit code: 3')).toBe(3)
		expect(exitCode([{ type: 'text', text: 'exit code: 127' }])).toBe(127)
		expect(exitCode('all good')).toBeUndefined()
	})
})
