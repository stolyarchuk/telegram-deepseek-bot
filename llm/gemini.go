package llm

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/yincongcyincong/mcp-client-go/clients"
	"github.com/yincongcyincong/telegram-deepseek-bot/conf"
	"github.com/yincongcyincong/telegram-deepseek-bot/db"
	"github.com/yincongcyincong/telegram-deepseek-bot/logger"
	"github.com/yincongcyincong/telegram-deepseek-bot/metrics"
	"github.com/yincongcyincong/telegram-deepseek-bot/param"
	"github.com/yincongcyincong/telegram-deepseek-bot/utils"
	"google.golang.org/genai"
)

type GeminiReq struct {
	ToolCall           []*genai.FunctionCall
	ToolMessage        []*genai.Content
	CurrentToolMessage []*genai.Content

	GeminiMsgs []*genai.Content
}

func (h *GeminiReq) CallLLMAPI(ctx context.Context, prompt string, l *LLM) error {
	_, _, userId := utils.GetChatIdAndMsgIdAndUserID(l.Update)

	h.GetMessages(userId, prompt)

	logger.Info("msg receive", "userID", userId, "prompt", l.Content)
	return h.Send(ctx, l)
}

func (h *GeminiReq) GetMessages(userId int64, prompt string) {
	messages := make([]*genai.Content, 0)

	msgRecords := db.GetMsgRecord(userId)
	if msgRecords != nil {
		aqs := msgRecords.AQs
		if len(aqs) > 10 {
			aqs = aqs[len(aqs)-10:]
		}
		for i, record := range aqs {
			if record.Answer != "" && record.Question != "" {
				logger.Info("context content", "dialog", i, "question:", record.Question,
					"toolContent", record.Content, "answer:", record.Answer)

				messages = append(messages, &genai.Content{
					Role: genai.RoleUser,
					Parts: []*genai.Part{
						{
							Text: record.Question,
						},
					},
				})

				messages = append(messages, &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							Text: record.Answer,
						},
					},
				})

			}
		}
	}

	h.GeminiMsgs = messages
}

func (h *GeminiReq) Send(ctx context.Context, l *LLM) error {
	if l.OverLoop() {
		return errors.New("too many loops")
	}

	start := time.Now()
	_, updateMsgID, userId := utils.GetChatIdAndMsgIdAndUserID(l.Update)
	h.GetModel(l)

	httpClient := utils.GetDeepseekProxyClient()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		HTTPClient: httpClient,
		APIKey:     *conf.GeminiToken,
	})
	if err != nil {
		logger.Error("init gemini client fail", "err", err)
		return err
	}

	config := &genai.GenerateContentConfig{
		TopP:             genai.Ptr[float32](float32(*conf.TopP)),
		FrequencyPenalty: genai.Ptr[float32](float32(*conf.FrequencyPenalty)),
		PresencePenalty:  genai.Ptr[float32](float32(*conf.PresencePenalty)),
		Temperature:      genai.Ptr[float32](float32(*conf.Temperature)),
		Tools:            l.GeminiTools,
	}

	chat, err := client.Chats.Create(ctx, l.Model, config, h.GeminiMsgs)
	if err != nil {
		logger.Error("create chat fail", "err", err)
		return err
	}

	msgInfoContent := &param.MsgInfo{
		SendLen: FirstSendLen,
	}

	hasTools := false
	for response, err := range chat.SendMessageStream(ctx, *genai.NewPartFromText(l.Content)) {
		if errors.Is(err, io.EOF) {
			logger.Info("stream finished", "updateMsgID", updateMsgID)
			break
		}
		if err != nil {
			logger.Error("stream error:", "updateMsgID", updateMsgID, "err", err)
			break
		}

		toolCalls := response.FunctionCalls()
		if len(toolCalls) > 0 {
			hasTools = true
			err = h.requestToolsCall(ctx, response)
			if err != nil {
				if errors.Is(err, ToolsJsonErr) {
					continue
				} else {
					logger.Error("requestToolsCall error", "updateMsgID", updateMsgID, "err", err)
				}
			}
		}

		if len(response.Text()) > 0 {
			msgInfoContent = l.sendMsg(msgInfoContent, response.Text())
		}

		if response.UsageMetadata != nil {
			l.Token += int(response.UsageMetadata.TotalTokenCount)
			metrics.TotalTokens.Add(float64(l.Token))
		}

	}

	if len(strings.TrimRightFunc(msgInfoContent.Content, unicode.IsSpace)) > 0 {
		l.MessageChan <- msgInfoContent
	}

	if !hasTools || len(h.CurrentToolMessage) == 0 {
		db.InsertMsgRecord(userId, &db.AQ{
			Question: l.Content,
			Answer:   l.WholeContent,
			Token:    l.Token,
		}, true)
	} else {
		h.ToolMessage = append(h.ToolMessage, h.CurrentToolMessage...)
		h.GeminiMsgs = append(h.GeminiMsgs, h.CurrentToolMessage...)
		h.CurrentToolMessage = make([]*genai.Content, 0)
		h.ToolCall = make([]*genai.FunctionCall, 0)
		return h.Send(ctx, l)
	}

	// record time costing in dialog
	totalDuration := time.Since(start).Seconds()
	metrics.ConversationDuration.Observe(totalDuration)
	return nil
}

func (h *GeminiReq) GetUserMessage(msg string) {
	h.GetMessage(genai.RoleUser, msg)
}

func (h *GeminiReq) GetAssistantMessage(msg string) {
	h.GetMessage(genai.RoleModel, msg)
}

func (h *GeminiReq) AppendMessages(client LLMClient) {
	if len(h.GeminiMsgs) == 0 {
		h.GeminiMsgs = make([]*genai.Content, 0)
	}

	h.GeminiMsgs = append(h.GeminiMsgs, client.(*GeminiReq).GeminiMsgs...)
}

func (h *GeminiReq) GetMessage(role, msg string) {
	if len(h.GeminiMsgs) == 0 {
		h.GeminiMsgs = []*genai.Content{
			{
				Role: role,
				Parts: []*genai.Part{
					{
						Text: msg,
					},
				},
			},
		}
		return
	}

	h.GeminiMsgs = append(h.GeminiMsgs, &genai.Content{
		Role: role,
		Parts: []*genai.Part{
			{
				Text: msg,
			},
		},
	})
}

func (h *GeminiReq) SyncSend(ctx context.Context, l *LLM) (string, error) {
	_, updateMsgID, _ := utils.GetChatIdAndMsgIdAndUserID(l.Update)
	h.GetModel(l)

	httpClient := utils.GetDeepseekProxyClient()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		HTTPClient: httpClient,
		APIKey:     *conf.GeminiToken,
	})
	if err != nil {
		logger.Error("init gemini client fail", "err", err)
		return "", err
	}

	config := &genai.GenerateContentConfig{
		TopP:             genai.Ptr[float32](float32(*conf.TopP)),
		FrequencyPenalty: genai.Ptr[float32](float32(*conf.FrequencyPenalty)),
		PresencePenalty:  genai.Ptr[float32](float32(*conf.PresencePenalty)),
		Temperature:      genai.Ptr[float32](float32(*conf.Temperature)),
		Tools:            l.GeminiTools,
	}

	chat, err := client.Chats.Create(ctx, l.Model, config, h.GeminiMsgs)
	if err != nil {
		logger.Error("create chat fail", "updateMsgID", updateMsgID, "err", err)
		return "", err
	}

	response, err := chat.Send(ctx, genai.NewPartFromText(l.Content))
	if err != nil {
		logger.Error("create chat fail", "err", err)
		return "", err
	}

	l.Token += int(response.UsageMetadata.TotalTokenCount)
	if len(response.FunctionCalls()) > 0 {
		h.requestOneToolsCall(ctx, response.FunctionCalls())
	}

	return response.Text(), nil
}

func (h *GeminiReq) requestOneToolsCall(ctx context.Context, toolsCall []*genai.FunctionCall) {
	for _, tool := range toolsCall {

		mc, err := clients.GetMCPClientByToolName(tool.Name)
		if err != nil {
			logger.Warn("get mcp fail", "err", err, "name", tool.Name, "args", tool.Args)
			return
		}

		toolsData, err := mc.ExecTools(ctx, tool.Name, tool.Args)
		if err != nil {
			logger.Warn("exec tools fail", "err", err, "name", tool.Name, "args", tool.Args)
			return
		}

		h.GeminiMsgs = append(h.GeminiMsgs, &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					FunctionCall: tool,
				},
			},
		})

		h.GeminiMsgs = append(h.GeminiMsgs, &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					FunctionResponse: &genai.FunctionResponse{
						Response: map[string]any{"output": toolsData},
						ID:       tool.ID,
						Name:     tool.Name,
					},
				},
			},
		})

		logger.Info("exec tool", "name", tool.Name, "args", tool.Args, "toolsData", toolsData)
	}
}

func (h *GeminiReq) requestToolsCall(ctx context.Context, response *genai.GenerateContentResponse) error {

	for _, toolCall := range response.FunctionCalls() {

		if toolCall.Name != "" {
			h.ToolCall = append(h.ToolCall, toolCall)
			h.ToolCall[len(h.ToolCall)-1].Name = toolCall.Name
		}

		if toolCall.ID != "" {
			h.ToolCall[len(h.ToolCall)-1].ID = toolCall.ID
		}

		if toolCall.Args != nil {
			h.ToolCall[len(h.ToolCall)-1].Args = toolCall.Args
		}

		mc, err := clients.GetMCPClientByToolName(h.ToolCall[len(h.ToolCall)-1].Name)
		if err != nil {
			logger.Warn("get mcp fail", "err", err)
			return err
		}

		toolsData, err := mc.ExecTools(ctx, h.ToolCall[len(h.ToolCall)-1].Name, h.ToolCall[len(h.ToolCall)-1].Args)
		if err != nil {
			logger.Warn("exec tools fail", "err", err)
			return err
		}
		h.CurrentToolMessage = append(h.CurrentToolMessage, &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					FunctionCall: toolCall,
				},
			},
		})

		h.CurrentToolMessage = append(h.CurrentToolMessage, &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					FunctionResponse: &genai.FunctionResponse{
						Response: map[string]any{"output": toolsData},
						ID:       h.ToolCall[len(h.ToolCall)-1].ID,
						Name:     h.ToolCall[len(h.ToolCall)-1].Name,
					},
				},
			},
		})
		logger.Info("send tool request", "function", toolCall.Name,
			"toolCall", toolCall.ID, "argument", toolCall.Args, "toolsData", toolsData)
	}

	return nil
}

func (h *GeminiReq) GetModel(l *LLM) {
	_, _, userId := utils.GetChatIdAndMsgIdAndUserID(l.Update)

	l.Model = param.ModelGemini20Flash
	userInfo, err := db.GetUserByID(userId)
	if err != nil {
		logger.Error("Error getting user info", "err", err)
	}
	if userInfo != nil && userInfo.Mode != "" && param.GeminiModels[userInfo.Mode] {
		logger.Info("User info", "userID", userInfo.UserId, "mode", userInfo.Mode)
		l.Model = userInfo.Mode
	}
}
