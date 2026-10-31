package core

import "strings"

const classifierInstructions = `You route a user's request in an AI coding/chat assistant to a model tier. Read the conversation excerpt and answer with ONE digit only:
1 = trivial: greeting, small talk, yes/no, translation, formatting, one-line factual answer
2 = light: simple explanation, short writing, tiny single-file code change, simple shell command
3 = standard: normal coding task, multi-step tool use, debugging a clear error, moderate writing/analysis
4 = hard: complex debugging, multi-file refactor, architecture/design, tricky algorithms, long careful reasoning
5 = hardest: research-grade reasoning, very large or ambiguous tasks, previous attempts clearly failed
If the latest message only says to continue, use the tier the task needs.
Also label the task with capability domains and weights that sum to 1 (use only the ones that matter):
design = UI/UX/product/interaction design; aesthetics = taste, visual or stylistic beauty and polish;
text = reading/understanding long input and writing clear output (docs, copy, translation, summaries);
logic = logical rigor and correctness of the reasoning/output (proofs, algorithms, edge cases, precise analysis);
backend = server/API/DB/infra/scripts code; frontend = web/mobile UI code (HTML/CSS/React…);
general = general knowledge and analysis; speed = quick, short, latency-sensitive answer.
Set u to true only if the latest message shows the user is unhappy with the previous assistant reply.
Answer with compact JSON only, no prose, e.g. {"tier":3,"w":{"backend":0.5,"logic":0.3,"frontend":0.2},"u":false}`

// BuildPrompt builds the classifier prompt (magpie-style clipping).
func BuildPrompt(t Turn) string {
	var b strings.Builder
	b.WriteString(classifierInstructions)
	b.WriteString("\n\n")
	if t.PrevUserText != "" {
		b.WriteString("<previous_user_message>\n")
		b.WriteString(Clip(t.PrevUserText, 1000, 0))
		b.WriteString("\n</previous_user_message>\n")
	}
	if t.LastAssistant != "" {
		b.WriteString("<last_assistant_reply>\n")
		b.WriteString(Clip(t.LastAssistant, 1000, 0))
		b.WriteString("\n</last_assistant_reply>\n")
	}
	b.WriteString("<latest_user_message>\n")
	b.WriteString(Clip(t.UserText, 3000, 1000))
	b.WriteString("\n</latest_user_message>\nJSON:")
	return b.String()
}
