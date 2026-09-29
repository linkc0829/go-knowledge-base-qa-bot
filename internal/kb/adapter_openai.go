package kb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

// ChatOptions are the decoding parameters sent with every chat completion.
// MaxTokens of 0 and an empty ReasoningEffort leave their fields off the
// request and let the upstream decide.
type ChatOptions struct {
	Temperature     float64
	MaxTokens       int64
	ReasoningEffort string
	ForwardUser     bool
}

// OpenAIClient implements LLM and Embedder.
type OpenAIClient struct {
	client     openai.Client
	chatModel  openai.ChatModel
	embedModel openai.EmbeddingModel
	baseURL    string
	chat       ChatOptions
}

func NewOpenAIClient(apiKey, baseURL, chatModel, embedModel string, chat ChatOptions) *OpenAIClient {
	opts := openAIOptions(apiKey, baseURL)
	return &OpenAIClient{
		client:     openai.NewClient(opts...),
		chatModel:  openai.ChatModel(chatModel),
		embedModel: openai.EmbeddingModel(embedModel),
		baseURL:    baseURL,
		chat:       chat,
	}
}

func (o *OpenAIClient) EmbedIdentity() string {
	return string(o.embedModel) + "@" + o.baseURL
}

func openAIOptions(apiKey, baseURL string) []option.RequestOption {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return opts
}

// ungroundedSentinel is the exact token the model must close a refusal with when
// it cannot answer from the context. The service strips it and sets
// Grounded=false, so callers get a structured signal instead of parsing the
// refusal prose — which proved unreliable across English, Chinese, and
// weak-model phrasings.
//
// It used to lead the reply, and that is why four questions failed across two
// eval suites with the right answer inside the refusal: asked which API 菜單設定
// calls, the model emitted the token, then listed DELETE /v1/menu/{0} and three
// more. A leading token forces the grounded decision before the answer exists,
// so a decision that depends on the answer cannot be made reliably — three
// rounds of prompt wording never fixed it. Closing with the token lets the model
// judge what it actually wrote.
const ungroundedSentinel = "[UNGROUNDED]"

const embeddingBatchSize = 32

// ungroundedPattern matches the sentinel tolerantly. A weak model asked for an
// exact token does not reliably produce one: llama3.1:8b emitted [UNEQUIPPED]
// for a refusal, which an exact prefix match missed, so the refusal was reported
// as grounded=true with citations attached. Accept any leading bracketed
// UN-token, and let common decoration (markdown emphasis, a code fence, an
// "Answer:" lead-in) sit in front of it.
//
// Best effort by construction: a model that refuses in plain prose with no token
// at all still reads as grounded. The deny path in service.go never consults the
// model, so it stays exact regardless.
// Trailing [*_:：,.、 -]* also consumes the closing emphasis and any punctuation
// joining the token to its reason, so the stripped text starts at the reason.
var ungroundedPattern = regexp.MustCompile(`^\s*(?:` + "```" + `[a-zA-Z]*\s*)?(?:[*_> ]*)(?:(?i:answer|答案|回答)\s*[:：]\s*)?[*_ ]*\[\s*UN[A-Z_ ]*\s*\][*_:：,.、 -]*`)

// ungroundedSuffixPattern matches the same token where the contract now puts it:
// last, after the reason. The bracket body must be UN plus capitals, which no
// citation can satisfy — those are file paths, lowercase and full of slashes —
// so a reply ending in [Store.POS/...#anchor] is untouched. A reply genuinely
// ending in a bracketed capitalised UN-word (an [UNDO] control label, say) would
// read as a refusal; no such label exists in this corpus, and the alternative,
// demanding the token sit alone on its own line, misses it whenever the model
// runs the token onto the end of its last sentence — which fails grounded=true,
// the unsafe direction.
var ungroundedSuffixPattern = regexp.MustCompile(`[\s*_>` + "`" + `]*\[\s*UN[A-Z_ ]*\s*\][\s*_:：,.、-]*$`)

// splitUngrounded reports whether text is an ungrounded refusal, returning the
// text with the sentinel removed. The trailing position is the contract; the
// leading one stays accepted because a model told to close with a token still
// sometimes opens with it, and reading that as an answer fails grounded=true.
func splitUngrounded(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if loc := ungroundedSuffixPattern.FindStringIndex(trimmed); loc != nil {
		return strings.TrimSpace(trimmed[:loc[0]]), true
	}
	if loc := ungroundedPattern.FindStringIndex(trimmed); loc != nil {
		return strings.TrimSpace(trimmed[loc[1]:]), true
	}
	return trimmed, false
}

const groundingSystem = `You answer questions ONLY using the provided context sections. ` +
	`Cite sources as filename#anchor. ` +
	`Only sections tagged evidence: procedure or procedure_visual may support an action order, steps, or sequence. ` +
	`Sections tagged ui_inventory, procedure_unlabeled, procedure_inferred, or general cannot support steps or sequence. ` +
	// The restriction above is about steps only, but the model kept generalising it into
	// "ui_inventory cannot answer anything": a retrieval probe found 47 of 52 refusals had
	// the right section in the top 3, most of them button-location questions answered by
	// ui_inventory. Say affirmatively what ui_inventory IS for, or the guardrail silently
	// suppresses the questions that section type exists to answer.
	`ui_inventory sections are the authoritative record of which controls a screen contains: ` +
	`when the question asks whether a control exists, what a screen contains, or which screen ` +
	`a control appears on, answer it directly from them — that is an inventory question, ` +
	`not a step or sequence question, and the steps-only restriction above does not apply. ` +
	`Reply in the same language as the question. ` +
	`A procedure_visual section is a coordinate-and-screenshot visual review, not a UIA-resolved control; identify it as visual evidence when relying on it. ` +
	`A procedure_unlabeled section proves only that a click occurred, never which control was clicked. ` +
	// Refuse on absence, not on wording. The model kept quoting the correct answer and
	// then declining: asked which API 點餐 calls, it printed POST /terminal/v1/order and
	// added "but the context does not explicitly define which one 點餐 itself maps to".
	// Reciting the verified symbol proves the evidence arrived; withholding it afterwards
	// is not caution, it is a wrong answer. This must not loosen must_not_infer — the
	// condition is that the answer is PRESENT, which says nothing about inventing one.
	`If the context contains a concrete answer — the symbol, endpoint, screen name, or method the question asks for — state it, even when the context does not repeat the question's own wording. ` +
	`Do not refuse on the grounds that a mapping is "not explicitly defined" while the context in fact supplies it; refuse when the answer is absent, never when it is present but phrased differently. ` +
	// gemma-4-26b-a4b summarises observable values away. Asked what the cart shows
	// after adding 紅茶 it answered "加入紅茶商品，字體顯示為紅色" and dropped the 70
	// printed in the very step it cited; asked how 未付 changes it called the numbers
	// absent while citing the two steps carrying 77 and 79. Retrieval was correct
	// both times, so the fix belongs here: an observable the answer does not name
	// is an observable the caller cannot act on.
	`When the question asks what is observable — a resulting state, an amount, a value shown on screen — reproduce those numbers and on-screen labels from the context verbatim rather than describing them; an answer that omits the value has not answered the question. ` +
	// The two triggers below used to be the whole list, and they left out the case
	// that matters most for a gap question: context squarely on topic that simply
	// never states the point asked for. Asked which of 平台覆寫 and 店家覆寫 wins,
	// the model retrieved the right 價目表 sections, answered that the documents do
	// not define a precedence — and kept grounded=true, because by the letter of the
	// rule it was not refusing. Six eval rounds reproduced it exactly. The last
	// sentence closes the gap the other way round: it ties the prose the model was
	// already writing to the token the caller reads, so the two cannot disagree.
	`Whenever you cannot answer the question from the context — the context is unrelated to the question, it only lists controls while the question asks for operation steps or order the knowledge base does not record, or the context is on topic yet never states the specific fact asked for, such as a precedence between two mechanisms it merely describes, a threshold, or a conflict rule — give a brief reason and end your reply with the exact token ` + ungroundedSentinel + `, and never invent or infer steps. ` +
	// Scoped to a reply that gives nothing back. Unscoped, it promoted any hedging
	// clause anywhere in a correct answer: asked how 未付 changes, the model quoted
	// 77 → 79 off the cited step, added that the context never states the arithmetic
	// behind it, and the sentence turned the whole reply into a refusal. Together
	// with the premise clause below, the rule is the same one twice: the token is
	// about the answer, not about what the context leaves unexplained around it.
	`If your reply would state that the context does not specify, define, or mention what was asked, and your reply therefore gives no answer to it, that reply is this case: it must end with the token. When your reply does state the answer, do not append that the context fails to explain or justify it — a fact the context records is an answer even where the context never explains why. ` +
	// The third trigger, added for the gap questions, immediately overreached: asked
	// what 桌位選擇 shows once 用餐方式 is 外帶, the model quoted 非內用無桌位可選 out
	// of the step that records it and then refused, because no section states 外帶 as
	// that step's precondition. It applied "the context never states it" to the
	// question's premise instead of to its answer. Same disease the two sentences
	// above treat, so name the boundary rather than let the triggers fight.
	`This trigger is about the answer being absent, not about a premise in the question being unstated: when the context shows the result the question asks for, answer it — never withhold it because the question's Given was not spelled out as a precondition.`

// GroundingFingerprint identifies the system prompt a running service answers
// with. Three eval rounds were spent measuring a service still running the
// previous prompt, because a metrics file named the model and its decoding
// parameters but nothing about the instructions. /health reports this, so a
// stale process is caught before a run rather than inferred from its results.
func GroundingFingerprint() string {
	sum := sha256.Sum256([]byte(groundingSystem))
	return hex.EncodeToString(sum[:6])
}

func (o *OpenAIClient) Answer(ctx context.Context, query string, sections []Section, history []Turn) (Completion, error) {
	messages := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(groundingSystem)}
	for _, turn := range history {
		messages = append(messages, openai.UserMessage(turn.Query), openai.AssistantMessage(turn.Answer))
	}
	messages = append(messages, openai.UserMessage(groundedPrompt(query, sections)))

	// Streamed only to time the first token: A5 compares backends on TTFT, which a
	// non-streaming call cannot see. The caller still gets the whole reply at once.
	params := openai.ChatCompletionNewParams{
		Messages:      messages,
		Model:         o.chatModel,
		Temperature:   openai.Float(o.chat.Temperature),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	if o.chat.MaxTokens > 0 {
		params.MaxTokens = openai.Int(o.chat.MaxTokens)
	}
	if o.chat.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(o.chat.ReasoningEffort)
	}
	var requestOptions []option.RequestOption
	if o.chat.ForwardUser {
		if principalID := principalIDFromContext(ctx); principalID != "" {
			requestOptions = append(requestOptions, option.WithHeader("X-On-Behalf-Of", principalID))
		}
	}
	start := time.Now()
	stream := o.client.Chat.Completions.NewStreaming(ctx, params, requestOptions...)
	defer stream.Close()
	var (
		out     Completion
		text    strings.Builder
		choices bool
	)
	for stream.Next() {
		chunk := stream.Current()
		if out.Model == "" {
			out.Model = chunk.Model
		}
		for _, choice := range chunk.Choices {
			choices = true
			if choice.Delta.Content != "" && out.TTFT == 0 {
				out.TTFT = time.Since(start)
			}
			text.WriteString(choice.Delta.Content)
		}
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			out.PromptTokens, out.CompletionTokens = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		}
	}
	if err := stream.Err(); err != nil {
		return Completion{}, classifyLLMError(fmt.Errorf("openai chat: %w", err))
	}
	out.Duration = time.Since(start)
	if out.Model == "" {
		out.Model = o.chatModel
	}
	// Returned for the per-question eval record and logged for daily sums; nothing
	// joins this line to a kb_query line.
	zap.L().Info("llm_usage",
		zap.String("model", o.chatModel),
		zap.Int64("prompt_tokens", out.PromptTokens),
		zap.Int64("completion_tokens", out.CompletionTokens),
		zap.Duration("ttft", out.TTFT),
		zap.Duration("duration", out.Duration),
	)
	if !choices {
		return Completion{}, fmt.Errorf("openai chat: no choices returned")
	}
	out.Text = strings.TrimSpace(text.String())
	return out, nil
}

func (o *OpenAIClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embeddingBatchSize {
		end := min(start+embeddingBatchSize, len(texts))
		batch, err := o.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("openai embed batch %d: %w", start/embeddingBatchSize+1, err)
		}
		vectors = append(vectors, batch...)
	}
	return vectors, nil
}

func (o *OpenAIClient) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	var requestOptions []option.RequestOption
	if o.chat.ForwardUser {
		if principalID := principalIDFromContext(ctx); principalID != "" {
			requestOptions = append(requestOptions, option.WithHeader("X-On-Behalf-Of", principalID))
		}
	}
	response, err := o.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: texts,
		},
		Model:          o.embedModel,
		EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
	}, requestOptions...)
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}

	vectors := make([][]float32, len(response.Data))
	for _, item := range response.Data {
		if item.Index < 0 || int(item.Index) >= len(vectors) {
			return nil, fmt.Errorf("openai embed: response index %d out of range", item.Index)
		}
		vec := make([]float32, len(item.Embedding))
		for i, value := range item.Embedding {
			vec[i] = float32(value)
		}
		vectors[item.Index] = vec
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("openai embed: got %d vectors, want %d", len(vectors), len(texts))
	}
	return vectors, nil
}

func groundedPrompt(query string, sections []Section) string {
	var b strings.Builder
	b.WriteString("Context sections:\n")
	for _, section := range sections {
		b.WriteString("\n[")
		b.WriteString(section.Citation())
		b.WriteString("] (evidence: ")
		b.WriteString(section.EvidenceClass())
		b.WriteString(")\n")
		b.WriteString(section.Body())
		b.WriteByte('\n')
	}
	b.WriteString("\nQuestion: ")
	b.WriteString(query)
	return b.String()
}

// classifyLLMError tags upstream failures the caller should retry rather than
// treat as a broken request. Only the adapter knows the SDK error shape, so the
// translation to package sentinels happens here and the handler stays free of
// SDK types.
func classifyLLMError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return NewLLMTransientError(ErrLLMUnavailable, "", err)
	}
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch {
	case apiErr.StatusCode == http.StatusTooManyRequests:
		return NewLLMTransientError(ErrLLMRateLimited, upstreamRetryAfter(apiErr), err)
	case apiErr.StatusCode >= 500:
		return NewLLMTransientError(ErrLLMUnavailable, upstreamRetryAfter(apiErr), err)
	}
	return err
}

func upstreamRetryAfter(apiErr *openai.Error) string {
	if apiErr.Response == nil {
		return ""
	}
	return apiErr.Response.Header.Get("Retry-After")
}
