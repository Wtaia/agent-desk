package runtime

import (
	"testing"
	"time"

	applicationruntime "agent-desk/internal/ai/application/runtime"
	"agent-desk/internal/models"
	"agent-desk/internal/pkg/enums"
)

func TestReplyEligibilityCanReply(t *testing.T) {
	eligibility := newReplyEligibility()
	conversation := newConversationFixture()
	message := newCustomerMessageFixture("hello")
	aiAgent := newAIAgentFixture()

	if !eligibility.CanReply(conversation, message, aiAgent) {
		t.Fatalf("expected customer message to be replyable")
	}

	message.SenderType = enums.IMSenderTypeAgent
	if eligibility.CanReply(conversation, message, aiAgent) {
		t.Fatalf("expected non-customer message to be rejected")
	}

	message = newCustomerMessageFixture("hello")
	conversation.HandoffAt = ptrTime(time.Now())
	if eligibility.CanReply(conversation, message, aiAgent) {
		t.Fatalf("expected handed-off conversation to be rejected")
	}

	conversation = newConversationFixture()
	conversation.CurrentAssigneeID = 1
	if eligibility.CanReply(conversation, message, aiAgent) {
		t.Fatalf("expected assigned conversation to be rejected")
	}

	conversation = newConversationFixture()
	aiAgent.ServiceMode = enums.IMConversationServiceModeHumanOnly
	if eligibility.CanReply(conversation, message, aiAgent) {
		t.Fatalf("expected human-only agent to be rejected")
	}

	aiAgent = newAIAgentFixture()
	message.Content = "   "
	if eligibility.CanReply(conversation, message, aiAgent) {
		t.Fatalf("expected blank message to be rejected")
	}

	message = models.Message{
		SenderType:  enums.IMSenderTypeCustomer,
		MessageType: enums.IMMessageTypeImage,
		Content:     "",
		Payload:     `{"assetId":"abc-123","provider":"local","storageKey":"images/photo.jpg"}`,
	}
	if !eligibility.CanReply(conversation, message, aiAgent) {
		t.Fatalf("expected image message with payload to be replyable")
	}
}

func TestResolveReplyTimeout(t *testing.T) {
	service := newAIReplyService()
	aiAgent := newAIAgentFixture()

	if got := service.resolveReplyTimeout(aiAgent); got != 180*time.Second {
		t.Fatalf("expected default timeout, got %v", got)
	}

	aiAgent.ReplyTimeoutSeconds = 30
	if got := service.resolveReplyTimeout(aiAgent); got != 30*time.Second {
		t.Fatalf("expected exact timeout, got %v", got)
	}

	aiAgent.ReplyTimeoutSeconds = 999
	if got := service.resolveReplyTimeout(aiAgent); got != 600*time.Second {
		t.Fatalf("expected clamped timeout, got %v", got)
	}
}

func TestResolveInterruptPrompt(t *testing.T) {
	summary := &applicationruntime.Summary{
		Interrupts: []applicationruntime.InterruptContextSummary{
			{
				ID:          "interrupt-1",
				Type:        "question",
				InfoPreview: `{"message":"请补充订单号"}`,
			},
		},
	}
	if got := resolveInterruptPrompt(summary); got != "请补充订单号" {
		t.Fatalf("unexpected interrupt prompt: %q", got)
	}

	summary.Interrupts[0].InfoPreview = "直接补充手机号"
	if got := resolveInterruptPrompt(summary); got != "直接补充手机号" {
		t.Fatalf("unexpected raw interrupt prompt: %q", got)
	}
}

func newConversationFixture() models.Conversation {
	return models.Conversation{}
}

func newCustomerMessageFixture(content string) models.Message {
	return models.Message{
		SenderType: enums.IMSenderTypeCustomer,
		Content:    content,
	}
}

func newAIAgentFixture() models.AIAgent {
	return models.AIAgent{}
}

func ptrTime(v time.Time) *time.Time {
	return &v
}
