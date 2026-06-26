package adapter

import (
	"agent-desk/internal/models"
	"agent-desk/internal/pkg/config"
	"agent-desk/internal/pkg/enums"
	"agent-desk/internal/pkg/utils"
	"agent-desk/internal/repositories"
	"agent-desk/internal/services/storage"
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/mlogclub/simple/sqls"
	ghtml "golang.org/x/net/html"
)

const defaultHistoryLimit = 12

type HistoryBuildResult struct {
	Messages []*schema.Message
	RawItems []models.Message
}

func BuildHistoryMessages(conversationID int64, currentMessageID int64, limit int) HistoryBuildResult {
	if conversationID <= 0 {
		return HistoryBuildResult{}
	}
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	items := repositories.MessageRepository.Find(sqls.DB(), sqls.NewCnd().
		Eq("conversation_id", conversationID).
		Desc("id").
		Limit(limit+1))
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	ret := HistoryBuildResult{
		Messages: make([]*schema.Message, 0, len(items)),
		RawItems: make([]models.Message, 0, len(items)),
	}
	for _, item := range items {
		if item.ID == currentMessageID {
			continue
		}
		msg := BuildSchemaMessage(&item)
		if msg == nil {
			continue
		}
		ret.RawItems = append(ret.RawItems, item)
		ret.Messages = append(ret.Messages, msg)
	}
	return ret
}

func BuildSchemaMessage(item *models.Message) *schema.Message {
	if item == nil {
		return nil
	}
	content := utils.BuildRuntimeMessageText(item.MessageType, item.Content)
	if content == "" {
		return nil
	}
	switch item.SenderType {
	case enums.IMSenderTypeCustomer:
		if imageURLs := ResolveMessageImageURLs(item); len(imageURLs) > 0 {
			return buildMultimodalUserMessage(content, imageURLs)
		}
		return schema.UserMessage(content)
	case enums.IMSenderTypeAI, enums.IMSenderTypeAgent:
		return schema.AssistantMessage(content, nil)
	default:
		return nil
	}
}

func BuildCurrentUserMessage(item *models.Message) *schema.Message {
	if item == nil {
		return nil
	}
	imageURLs := ResolveMessageImageURLs(item)
	content := utils.BuildRuntimeMessageText(item.MessageType, item.Content)
	if content == "" && len(imageURLs) == 0 {
		return nil
	}
	if len(imageURLs) > 0 {
		return buildMultimodalUserMessage(content, imageURLs)
	}
	return schema.UserMessage(content)
}

func buildMultimodalUserMessage(text string, imageURLs []string) *schema.Message {
	parts := make([]schema.MessageInputPart, 0, 1+len(imageURLs))
	if strings.TrimSpace(text) != "" {
		parts = append(parts, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeText,
			Text: text,
		})
	}
	for _, rawURL := range imageURLs {
		url := rawURL
		parts = append(parts, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{
					URL: &url,
				},
			},
		})
	}
	if len(parts) == 0 {
		return nil
	}
	return &schema.Message{
		Role:                    schema.User,
		UserInputMultiContent: parts,
	}
}

type assetPayload struct {
	AssetID    string              `json:"assetId"`
	Provider   enums.AssetProvider `json:"provider,omitempty"`
	StorageKey string              `json:"storageKey,omitempty"`
}

func ResolveMessageImageURLs(item *models.Message) []string {
	if item == nil {
		return nil
	}
	switch item.MessageType {
	case enums.IMMessageTypeImage:
		if url := resolveAssetPayloadURL(item.Payload); url != "" {
			return []string{toAbsoluteURL(url)}
		}
	case enums.IMMessageTypeHTML:
		urls := resolveHTMLImageURLs(item.Content)
		for i, url := range urls {
			urls[i] = toAbsoluteURL(url)
		}
		return urls
	}
	return nil
}

func toAbsoluteURL(url string) string {
	url = strings.TrimSpace(url)
	if url == "" || strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.Current().Server.BaseURL), "/")
	if baseURL == "" {
		return url
	}
	if strings.HasPrefix(url, "/") {
		return baseURL + url
	}
	return baseURL + "/" + url
}

func resolveAssetPayloadURL(payload string) string {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return ""
	}
	var asset assetPayload
	if err := json.Unmarshal([]byte(payload), &asset); err != nil {
		return ""
	}
	asset.AssetID = strings.TrimSpace(asset.AssetID)
	asset.Provider = enums.AssetProvider(strings.TrimSpace(string(asset.Provider)))
	asset.StorageKey = strings.TrimSpace(asset.StorageKey)
	if asset.Provider == "" || asset.StorageKey == "" {
		if asset.AssetID == "" {
			return ""
		}
		dbAsset := repositories.AssetRepository.GetByAssetID(sqls.DB(), asset.AssetID)
		if dbAsset == nil {
			return ""
		}
		if asset.Provider == "" {
			asset.Provider = dbAsset.Provider
		}
		if asset.StorageKey == "" {
			asset.StorageKey = strings.TrimSpace(dbAsset.StorageKey)
		}
	}
	if asset.Provider == "" || asset.StorageKey == "" {
		return ""
	}
	provider, err := storage.NewProvider(asset.Provider)
	if err != nil {
		return ""
	}
	return provider.GetSignedURL(asset.StorageKey)
}

func resolveHTMLImageURLs(content string) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	doc, err := ghtml.Parse(strings.NewReader("<div>" + content + "</div>"))
	if err != nil {
		return nil
	}
	var urls []string
	var walk func(*ghtml.Node)
	walk = func(node *ghtml.Node) {
		if node == nil {
			return
		}
		if node.Type == ghtml.ElementNode && node.Data == "img" {
			provider := enums.AssetProvider(strings.TrimSpace(findAttr(node, "data-provider")))
			storageKey := strings.TrimSpace(findAttr(node, "data-storage-key"))
			if provider != "" && storageKey != "" {
				if p, err := storage.NewProvider(provider); err == nil {
					if url := p.GetSignedURL(storageKey); url != "" {
						urls = append(urls, url)
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return urls
}

func findAttr(node *ghtml.Node, key string) string {
	if node == nil {
		return ""
	}
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}
