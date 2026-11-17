// Amp plugin used by the offline evaluation to answer brisk host requests
// with Jev through amp.ai.decide, the same route the Amp adapter uses.
// Load it into a thread with load_plugin, then call brisk_decide_batch.
import type { PluginAPI } from '@ampcode/plugin'
import { appendFileSync, existsSync, readFileSync } from 'node:fs'

export const description = 'Evaluation helper: answers brisk host requests in a JSONL file with amp.ai.decide (Jev).'

export default function (amp: PluginAPI) {
	amp.registerTool({
		name: 'brisk_decide_batch',
		description:
			'Answer brisk host requests with amp.ai.decide. input: JSONL of {key, request_id, state, questions}. output: JSONL appended with {key, request_id, model, answers, ms} or {key, error}. Keys already in output are skipped, so it can be rerun to resume.',
		inputSchema: {
			type: 'object',
			properties: {
				input: { type: 'string' },
				output: { type: 'string' },
				concurrency: { type: 'number' },
				limit: { type: 'number', description: 'Answer at most this many new requests in this call' },
			},
			required: ['input', 'output'],
		},
		async execute(input) {
			const inPath = String(input.input)
			const outPath = String(input.output)
			const concurrency = Number(input.concurrency ?? 8)
			const limit = Number(input.limit ?? 1e9)
			const done = new Set<string>()
			if (existsSync(outPath)) {
				for (const line of readFileSync(outPath, 'utf8').split('\n')) {
					if (!line.trim()) continue
					const r = JSON.parse(line)
					if (!r.error) done.add(r.key)
				}
			}
			const todo = readFileSync(inPath, 'utf8')
				.split('\n')
				.filter((l) => l.trim())
				.map((l) => JSON.parse(l))
				.filter((r) => !done.has(r.key))
				.slice(0, limit)
			let ok = 0
			let failed = 0
			let next = 0
			const started = Date.now()
			async function worker() {
				while (next < todo.length) {
					const r = todo[next++]
					const t = performance.now()
					try {
						const res = await amp.ai.decide({ model: 'typesafe/jev', state: r.state, questions: r.questions })
						appendFileSync(
							outPath,
							JSON.stringify({ key: r.key, request_id: r.request_id, model: res.model, answers: res.answers, ms: Math.round(performance.now() - t) }) + '\n',
						)
						ok++
					} catch (e) {
						appendFileSync(outPath, JSON.stringify({ key: r.key, request_id: r.request_id, error: String(e) }) + '\n')
						failed++
					}
				}
			}
			await Promise.all(Array.from({ length: Math.max(1, concurrency) }, worker))
			return `answered ${ok}, failed ${failed}, skipped ${done.size} already answered, ${todo.length - ok - failed} left in this call; ${Math.round((Date.now() - started) / 1000)} s`
		},
	})
}
