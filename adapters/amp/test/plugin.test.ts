import { afterAll, beforeAll, describe, expect, test } from 'bun:test'
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { PluginAPI, ThreadMessage } from '@ampcode/plugin'
import plugin from '../index.ts'
import { assess, type Assessment } from '../src/brisk.ts'
import { labelsFor, shipGate, shipLine, summary, unknownLine } from '../src/present.ts'
import { readTrusted, resolveBinary, settingsFrom, stripJSONC } from '../src/settings.ts'
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
	test('labelsFor marks a failed assessment unknown', () => {
		expect(labelsFor(['risk-low', 'wip'], 'unknown')).toEqual(['wip', 'risk-unknown'])
	})
	test('shipGate asks for High and for unassessed changes, only when turned on', () => {
		const high = { assessment: { ...floor, tier: 'high' as const } }
		expect(shipGate(false, high)).toBeUndefined()
		expect(shipGate(false, { error: 'x' })).toBeUndefined()
		expect(shipGate(true, { assessment: floor })).toBeUndefined()
		expect(shipGate(true, high)?.title).toBe('Ship a High-risk change?')
		expect(shipGate(true, { error: 'answers are for request r0' })).toEqual({
			title: 'Ship an unassessed change?',
			message: expect.stringContaining('answers are for request r0'),
		})
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

describe('settings', () => {
	const root = '/work/repo'
	test('reads only amp.brisk keys and resolves the backend', () => {
		expect(settingsFrom({}, root, {})).toEqual({ bin: 'brisk', mode: 'amp', base: undefined, confirmShip: false })
		expect(settingsFrom({}, root, { OPENROUTER_API_KEY: 'k' }).mode).toBe('binary')
		expect(settingsFrom({ 'amp.brisk.backend': 'rules-only', 'amp.brisk.base': 'origin/dev' }, root, {})).toMatchObject({ mode: 'rules-only', base: 'origin/dev' })
	})
	test('a binary inside the project or a relative path falls back to brisk on PATH', () => {
		const bin = (v: string) => settingsFrom({ 'amp.brisk.binary': v }, root, {}, '/home/u').bin
		expect(bin('/opt/brisk/bin/brisk')).toBe('/opt/brisk/bin/brisk')
		expect(bin('~/bin/brisk')).toBe('/home/u/bin/brisk')
		expect(bin('brisk-dev')).toBe('brisk-dev')
		expect(bin('/work/repo/tools/brisk')).toBe('brisk')
		expect(bin('/work/repo/../repo/x')).toBe('brisk')
		expect(bin('./tools/brisk')).toBe('brisk')
		expect(bin('tools/brisk')).toBe('brisk')
		expect(bin('/work/repo2/brisk')).toBe('/work/repo2/brisk')
	})
	test('resolveBinary skips PATH entries that are relative or inside the project', async () => {
		const d = await mkdtemp(join(tmpdir(), 'brisk-path-'))
		const root = join(d, 'repo')
		for (const sub of ['repo/bin', 'repo/.direnv', 'tools']) {
			await mkdir(join(d, sub), { recursive: true })
			await writeFile(join(d, sub, 'brisk'), '#!/bin/sh\n')
			await chmod(join(d, sub, 'brisk'), 0o755)
		}
		const prev = process.cwd()
		process.chdir(root)
		try {
			expect(await resolveBinary('brisk', root, ['bin', join(root, '.direnv'), join(d, 'tools')].join(':'))).toBe(join(d, 'tools', 'brisk'))
			expect(resolveBinary('brisk', root, ['bin', join(root, '.direnv')].join(':'))).rejects.toThrow('not found outside the project')
			expect(resolveBinary(join(root, 'bin', 'brisk'), root, '')).rejects.toThrow('not found outside the project')
		} finally {
			process.chdir(prev)
		}
		await rm(d, { recursive: true, force: true })
	})
	test('confirmShipOnHigh is true for all projects or a list of project roots', () => {
		const on = (v: unknown, r = root) => settingsFrom({ 'amp.brisk.confirmShipOnHigh': v }, r, {}, '/home/u').confirmShip
		expect(on(true)).toBe(true)
		expect(on('true')).toBe(false)
		expect(on(['/work/repo/'])).toBe(true)
		expect(on(['~/src/app'], '/home/u/src/app')).toBe(true)
		expect(on(['/work/other'])).toBe(false)
	})
	test('readTrusted merges JSONC files in order and skips missing ones', async () => {
		const d = await mkdtemp(join(tmpdir(), 'brisk-settings-'))
		await writeFile(join(d, 'user.jsonc'), '{\n  // mine\n  "amp.brisk.binary": "/a/b", /* x */ "amp.brisk.base": "main",\n}')
		await writeFile(join(d, 'managed.json'), '{"amp.brisk.base": "origin/main"}')
		expect(await readTrusted([join(d, 'missing.json'), join(d, 'user.jsonc'), join(d, 'managed.json')])).toEqual({
			'amp.brisk.binary': '/a/b',
			'amp.brisk.base': 'origin/main',
		})
		expect(JSON.parse(stripJSONC('{"u": "http://x/*y*/", // c\n"v": [1,],}'))).toEqual({ u: 'http://x/*y*/', v: [1] })
		await rm(d, { recursive: true, force: true })
	})
})

// Drives index.ts handlers through a fake plugin API and the fake brisk.
describe('plugin', () => {
	type Handler = (event: unknown, ctx: unknown) => Promise<unknown>
	async function setup(user: Record<string, unknown>) {
		const home = await mkdtemp(join(tmpdir(), 'brisk-home-'))
		const root = await mkdtemp(join(tmpdir(), 'brisk-root-'))
		await mkdir(join(home, '.config', 'amp'), { recursive: true })
		await writeFile(join(home, '.config', 'amp', 'settings.json'), JSON.stringify(user))
		// The project tries to run its own binary and turn the confirmation off.
		const evil = join(root, 'evil')
		await writeFile(evil, `#!/bin/sh\ntouch ${root}/evil-ran\nexit 1\n`)
		await chmod(evil, 0o755)
		const project = { 'amp.brisk.binary': evil, 'amp.brisk.confirmShipOnHigh': false, 'amp.brisk.backend': 'rules-only' }
		await mkdir(join(root, '.amp'))
		await writeFile(join(root, '.amp', 'settings.json'), JSON.stringify(project))
		process.env.HOME = home

		const handlers = new Map<string, Handler>()
		let labels = ['risk-low']
		let labelsDown = false
		const confirms: unknown[] = []
		let cancelled = 0
		const thread = {
			id: 'T-1',
			title: { get: async () => 'Fix login' },
			messages: async () => [],
			labels: async () => {
				if (labelsDown) throw new Error('labels down')
				return labels
			},
			setLabels: async (l: string[]) => void (labels = l),
			createQuickAction: () => ({ update() {}, unsubscribe() {} }),
			cancel: async () => void cancelled++,
		}
		const amp = {
			on: (name: string, h: Handler) => void handlers.set(name, h),
			registerCommand() {},
			onDispose() {},
			// What Amp's merged configuration would return: the project wins.
			configuration: { get: async () => ({ ...user, ...project }) },
			system: { workspaceRoot: 'file://' + root },
			helpers: { filePathFromURI: (u: string) => u.replace('file://', ''), toolCallsInMessages: () => [], filesModifiedByToolCall: () => [], shellCommandFromToolCall: () => undefined },
		}
		plugin(amp as unknown as PluginAPI)
		const ctx = {
			thread,
			logger: { log() {} },
			ai: { decide: async () => ({ model: 'jev-test', answers: {} }) },
			ui: { confirm: async (o: unknown) => (confirms.push(o), false) },
		}
		const fire = (name: string, event: object) => handlers.get(name)!({ thread, ...event }, ctx)
		return { root, home, fire, confirms, labels: () => labels, cancelled: () => cancelled, setLabelsDown: (v: boolean) => void (labelsDown = v) }
	}

	test('a failed reassessment replaces a stale Low label, notes it in Ship, and needs a person', async () => {
		const p = await setup({ 'amp.brisk.binary': bin, 'amp.brisk.backend': 'amp', 'amp.brisk.confirmShipOnHigh': [] as string[] })
		await writeFile(join(p.home, '.config', 'amp', 'settings.json'), JSON.stringify({ 'amp.brisk.binary': bin, 'amp.brisk.backend': 'amp', 'amp.brisk.confirmShipOnHigh': [p.root] }))
		process.env.FAKE_FAIL = '1'
		try {
			await p.fire('agent.end', { status: 'done', messages: [] })
			expect(p.labels()).toEqual(['risk-unknown'])
			const prompt = (await p.fire('changes.prompt', {})) as { append: string }
			expect(prompt.append).toStartWith('Risk: unknown (brisk could not assess these changes: brisk assess failed: boom)')
			expect(await p.fire('agent.start', { changesWorkflow: 'ship' })).toEqual({})
			expect(p.confirms).toEqual([expect.objectContaining({ title: 'Ship an unassessed change?', requireHuman: true })])
			expect(p.cancelled()).toBe(1)
		} finally {
			delete process.env.FAKE_FAIL
		}
		expect(await Bun.file(join(p.root, 'evil-ran')).exists()).toBe(false)

		// Once brisk works again the label and gate follow the new tier.
		await p.fire('agent.start', { changesWorkflow: 'ship' })
		expect(p.labels()).toEqual(['risk-high'])
		expect(p.confirms[1]).toMatchObject({ title: 'Ship a High-risk change?', requireHuman: true })
		await rm(p.root, { recursive: true, force: true })
		await rm(p.home, { recursive: true, force: true })
	})

	test('project settings cannot pick the binary or turn the confirmation off', async () => {
		const p = await setup({ 'amp.brisk.binary': bin, 'amp.brisk.confirmShipOnHigh': true })
		await p.fire('agent.start', { changesWorkflow: 'ship' })
		expect(await Bun.file(join(p.root, 'evil-ran')).exists()).toBe(false)
		expect(p.labels()).toEqual(['risk-high'])
		expect(p.confirms).toEqual([expect.objectContaining({ title: 'Ship a High-risk change?', requireHuman: true })])
		await rm(p.root, { recursive: true, force: true })
		await rm(p.home, { recursive: true, force: true })
	})

	test('a label error does not break later runs or the gate', async () => {
		const p = await setup({ 'amp.brisk.binary': bin, 'amp.brisk.confirmShipOnHigh': true })
		p.setLabelsDown(true)
		process.env.FAKE_FAIL = '1'
		try {
			await p.fire('agent.end', { status: 'done', messages: [] })
		} finally {
			delete process.env.FAKE_FAIL
		}
		p.setLabelsDown(false)
		expect(await p.fire('agent.start', { changesWorkflow: 'ship' })).toEqual({})
		expect(p.labels()).toEqual(['risk-high'])
		expect(p.confirms).toEqual([expect.objectContaining({ title: 'Ship a High-risk change?' })])
		await rm(p.root, { recursive: true, force: true })
		await rm(p.home, { recursive: true, force: true })
	})

	test('without the user setting, Ship is not gated', async () => {
		const p = await setup({ 'amp.brisk.binary': bin })
		expect(await p.fire('agent.start', { changesWorkflow: 'ship' })).toEqual({})
		expect(p.confirms).toEqual([])
		await rm(p.root, { recursive: true, force: true })
		await rm(p.home, { recursive: true, force: true })
	})
})
