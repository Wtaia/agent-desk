package executor

import (
	"context"
	"strings"
	"time"

	"agent-desk/internal/ai/runtime/internal/impl/callbacks"
	"agent-desk/internal/ai/runtime/internal/impl/retrievers"
	"agent-desk/internal/models"
	"agent-desk/internal/pkg/utils"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

const (
	answerabilityNodeRetrieve = "retrieve_knowledge"
	answerabilityNodeAllow    = "allow_agent"
	answerabilityNodeFallback = "fallback"

	answerabilityStatusSkipped      = "skipped"
	answerabilityStatusNoContext    = "no_context"
	answerabilityStatusHasContext   = "has_context"
	answerabilityStatusUnanswerable = "unanswerable"
)

type knowledgeContextRetriever interface {
	KnowledgeBaseIDs() []int64
	RetrieveContextByOptions(ctx context.Context, opts retrievers.KnowledgeRetrieveOptions, query string) (*retrievers.KnowledgeRetrieveResult, error)
}

type answerabilityRetrieverFactory func(aiAgent models.AIAgent) knowledgeContextRetriever

type KnowledgeAnswerabilityGate struct {
	newRetriever answerabilityRetrieverFactory
}

type answerabilityGateInput struct {
	Request   RunInput
	Summary   *RunResult
	Collector *callbacks.RuntimeTraceCollector
	Messages  []*schema.Message
}

type answerabilityGateState struct {
	Input          answerabilityGateInput
	KnowledgeIDs   []int64
	RetrieveResult *retrievers.KnowledgeRetrieveResult
	Decision       knowledgeGuardDecision
	SkipGate       bool
	FallbackReply  string
	ErrorMessage   string
}

func NewKnowledgeAnswerabilityGate() *KnowledgeAnswerabilityGate {
	return &KnowledgeAnswerabilityGate{
		newRetriever: func(aiAgent models.AIAgent) knowledgeContextRetriever {
			return retrievers.NewKnowledgeRetriever(aiAgent)
		},
	}
}

func (g *KnowledgeAnswerabilityGate) withDefaults() *KnowledgeAnswerabilityGate {
	if g == nil {
		return NewKnowledgeAnswerabilityGate()
	}
	ret := *g
	defaults := NewKnowledgeAnswerabilityGate()
	if ret.newRetriever == nil {
		ret.newRetriever = defaults.newRetriever
	}
	return &ret
}

func (g *KnowledgeAnswerabilityGate) Evaluate(ctx context.Context, input answerabilityGateInput) (*answerabilityGateState, error) {
	gate := g.withDefaults()
	graph := compose.NewGraph[*answerabilityGateState, *answerabilityGateState]()
	if err := graph.AddLambdaNode(answerabilityNodeRetrieve, compose.InvokableLambda(gate.retrieveKnowledge)); err != nil {
		return nil, err
	}
	if err := graph.AddLambdaNode(answerabilityNodeAllow, compose.InvokableLambda(allowAnswerabilityPassThrough)); err != nil {
		return nil, err
	}
	if err := graph.AddLambdaNode(answerabilityNodeFallback, compose.InvokableLambda(fallbackAnswerabilityPassThrough)); err != nil {
		return nil, err
	}
	if err := graph.AddEdge(compose.START, answerabilityNodeRetrieve); err != nil {
		return nil, err
	}
	if err := graph.AddBranch(answerabilityNodeRetrieve, compose.NewGraphBranch(routeAnswerabilityGate, map[string]bool{
		answerabilityNodeAllow:    true,
		answerabilityNodeFallback: true,
	})); err != nil {
		return nil, err
	}
	if err := graph.AddEdge(answerabilityNodeAllow, compose.END); err != nil {
		return nil, err
	}
	if err := graph.AddEdge(answerabilityNodeFallback, compose.END); err != nil {
		return nil, err
	}
	runnable, err := graph.Compile(ctx)
	if err != nil {
		return nil, err
	}
	return runnable.Invoke(ctx, &answerabilityGateState{Input: input})
}

func routeAnswerabilityGate(ctx context.Context, state *answerabilityGateState) (string, error) {
	if state == nil {
		return answerabilityNodeFallback, nil
	}
	if state.SkipGate || strings.TrimSpace(state.FallbackReply) == "" {
		return answerabilityNodeAllow, nil
	}
	return answerabilityNodeFallback, nil
}

func allowAnswerabilityPassThrough(ctx context.Context, state *answerabilityGateState) (*answerabilityGateState, error) {
	if state == nil {
		return &answerabilityGateState{}, nil
	}
	if len(state.Decision.Instructions) > 0 {
		state.Input.Messages = append(state.Input.Messages, state.Decision.Instructions...)
	}
	if state.RetrieveResult != nil {
		if contextText := strings.TrimSpace(state.RetrieveResult.ContextText); contextText != "" {
			state.Input.Messages = append(state.Input.Messages, schema.SystemMessage(contextText))
		}
	}
	return state, nil
}

func fallbackAnswerabilityPassThrough(ctx context.Context, state *answerabilityGateState) (*answerabilityGateState, error) {
	if state == nil {
		return &answerabilityGateState{}, nil
	}
	return state, nil
}

func (g *KnowledgeAnswerabilityGate) retrieveKnowledge(ctx context.Context, state *answerabilityGateState) (*answerabilityGateState, error) {
	if state == nil {
		state = &answerabilityGateState{}
	}
	gate := g.withDefaults()
	req := state.Input.Request
	messageText := utils.BuildRuntimeMessageText(req.UserMessage.MessageType, req.UserMessage.Content)
	if isRuntimeActionIntent(messageText) {
		state.SkipGate = true
		state.recordAnswerability(answerabilityStatusSkipped, "runtime action intent", nil)
		return state, nil
	}
	configuredKnowledgeIDs := utils.SplitInt64s(req.AIAgent.KnowledgeIDs)
	if len(configuredKnowledgeIDs) == 0 {
		state.SkipGate = true
		state.recordAnswerability(answerabilityStatusSkipped, "no knowledge configured", nil)
		return state, nil
	}
	retriever := gate.newRetriever(req.AIAgent)
	state.KnowledgeIDs = append([]int64(nil), configuredKnowledgeIDs...)
	if retriever == nil {
		state.FallbackReply = resolveKnowledgeHumanSupportFallback(req.AIAgent)
		state.recordAnswerability(answerabilityStatusUnanswerable, "knowledge retriever unavailable", nil)
		return state, nil
	}
	knowledgeIDs := retriever.KnowledgeBaseIDs()
	state.KnowledgeIDs = append([]int64(nil), knowledgeIDs...)
	if len(knowledgeIDs) == 0 {
		state.SkipGate = true
		state.recordAnswerability(answerabilityStatusSkipped, "no knowledge configured", nil)
		return state, nil
	}
	query := strings.TrimSpace(messageText)
	if query == "" {
		state.Decision = buildKnowledgeNoContextDecision(req.AIAgent, knowledgeIDs)
		state.recordAnswerability(answerabilityStatusNoContext, "empty user question", nil)
		return state, nil
	}
	retrieveOptions := retrievers.DefaultKnowledgeRetrieveOptions()
	retrieveOptions.QueryPreview = preview(messageText, 120)
	result, err := retriever.RetrieveContextByOptions(ctx, retrieveOptions, query)
	if err != nil {
		state.Decision = buildKnowledgeRetrievalErrorDecision(req.AIAgent, knowledgeIDs)
		state.ErrorMessage = err.Error()
		state.recordAnswerability(answerabilityStatusUnanswerable, "knowledge retrieval failed", err)
		return state, nil
	}
	state.RetrieveResult = result
	if state.Input.Summary != nil && result != nil {
		state.Input.Summary.RetrieverCount = len(result.Hits)
	}
	if state.Input.Collector != nil && result != nil {
		state.Input.Collector.SetRetrieverSummary(result.TraceSummary)
		state.Input.Collector.AddRetrieverItems(result.TraceItems)
	}
	if result == nil || len(result.Hits) == 0 || strings.TrimSpace(result.ContextText) == "" {
		state.Decision = buildKnowledgeNoContextDecision(req.AIAgent, knowledgeIDs)
		state.recordAnswerability(answerabilityStatusNoContext, "no retrieved context", nil)
		return state, nil
	}
	state.Decision = buildKnowledgeGuardDecision(req.AIAgent, result)
	state.recordAnswerability(answerabilityStatusHasContext, "retrieved context injected", nil)
	return state, nil
}

func isRuntimeActionIntent(content string) bool {
	text := strings.ToLower(strings.TrimSpace(content))
	if text == "" {
		return false
	}
	compact := strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "").Replace(text)
	handoffPhrases := []string{
		"我要转人工",
		"帮我转人工",
		"转人工",
		"接人工",
		"找人工",
		"真人客服",
		"humanagent",
		"liveagent",
	}
	for _, phrase := range handoffPhrases {
		if strings.Contains(compact, phrase) {
			return true
		}
	}
	if containsAny(compact, []string{"人工客服", "人工服务", "人工处理"}) &&
		!containsAny(compact, []string{"是什么", "怎么", "如何", "多少", "几", "吗", "?"}) &&
		(isShortActionPhrase(compact) || containsAny(compact, []string{"我要", "帮我", "请", "联系", "需要"})) {
		return true
	}
	ticketPhrases := []string{
		"创建工单",
		"新建工单",
		"提交工单",
		"发起工单",
		"建工单",
		"开工单",
		"我要建单",
		"帮我建单",
		"创建ticket",
		"createticket",
	}
	for _, phrase := range ticketPhrases {
		if strings.Contains(compact, phrase) {
			return true
		}
	}
	if strings.Contains(compact, "工单") {
		for _, action := range []string{"创建", "新建", "提交", "发起", "建", "开", "帮我", "我要", "请"} {
			if strings.Contains(compact, action) {
				return true
			}
		}
	}
	return false
}

func containsAny(text string, values []string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func isShortActionPhrase(text string) bool {
	return len([]rune(text)) <= 8
}

func (s *answerabilityGateState) recordAnswerability(status string, reason string, err error) {
	s.recordAnswerabilityWithLatency(status, reason, err, time.Time{})
}

func (s *answerabilityGateState) recordAnswerabilityWithLatency(status string, reason string, err error, started time.Time) {
	if s == nil || s.Input.Collector == nil {
		return
	}
	errorMessage := strings.TrimSpace(s.ErrorMessage)
	if err != nil {
		errorMessage = err.Error()
	}
	data := callbacks.AnswerabilityTraceData{
		Status:       status,
		Reason:       strings.TrimSpace(reason),
		ErrorMessage: errorMessage,
	}
	if !started.IsZero() {
		data.LatencyMs = time.Since(started).Milliseconds()
	}
	s.Input.Collector.SetAnswerability(data)
}
