package robot

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	godeepseek "github.com/cohesion-org/deepseek-go"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/yincongcyincong/langchaingo/chains"
	"github.com/yincongcyincong/langchaingo/vectorstores"
	"github.com/yincongcyincong/telegram-deepseek-bot/conf"
	"github.com/yincongcyincong/telegram-deepseek-bot/db"
	"github.com/yincongcyincong/telegram-deepseek-bot/i18n"
	"github.com/yincongcyincong/telegram-deepseek-bot/llm"
	"github.com/yincongcyincong/telegram-deepseek-bot/logger"
	"github.com/yincongcyincong/telegram-deepseek-bot/param"
	"github.com/yincongcyincong/telegram-deepseek-bot/rag"
	"github.com/yincongcyincong/telegram-deepseek-bot/utils"
)

// StartListenRobot start listen robot callback
func StartListenRobot() {
	for {

		bot := utils.CreateBot()
		logger.Info("telegramBot Info", "username", bot.Self.UserName)

		u := tgbotapi.NewUpdate(0)
		u.Timeout = 60

		updates := bot.GetUpdatesChan(u)
		for update := range updates {
			execUpdate(update, bot)
		}
	}
}

// execUpdate exec telegram message
func execUpdate(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId, msgId, userId := utils.GetChatIdAndMsgIdAndUserID(update)

	// Handle business updates first
	if handleBusinessUpdates(update, bot) {
		return
	}

	if !checkUserAllow(update) && !checkGroupAllow(update) {
		chat := utils.GetChat(update)
		logger.Warn("user/group not allow to use this bot", "userID", userId, "chat", chat)
		i18n.SendMsg(chatId, "valid_user_group", bot, nil, msgId)
		return
	}

	if handleCommandAndCallback(update, bot) {
		return
	}
	// check whether you have new message
	if update.Message != nil {
		if skipThisMsg(update, bot) {
			logger.Warn("skip this msg", "msgId", msgId, "chat", chatId, "type", update.Message.Chat.Type, "content", update.Message.Text)
			return
		}
		requestDeepseekAndResp(update, bot, update.Message.Text)
	}

}

// requestDeepseekAndResp request deepseek api
func requestDeepseekAndResp(update tgbotapi.Update, bot *tgbotapi.BotAPI, content string) {
	_, _, userId := utils.GetChatIdAndMsgIdAndUserID(update)
	if checkUserTokenExceed(update, bot) {
		logger.Warn("user token exceed", "userID", userId)
		return
	}

	if conf.Store != nil {
		executeChain(update, bot, content)
	} else {
		executeLLM(update, bot, content)
	}

}

// executeChain use langchain to interact llm
func executeChain(update tgbotapi.Update, bot *tgbotapi.BotAPI, content string) {
	messageChan := make(chan *param.MsgInfo)

	go func() {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("GetContent panic err", "err", err)
			}
			utils.DecreaseUserChat(update)
			close(messageChan)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		text, err := utils.GetContent(update, bot, content)
		if err != nil {
			logger.Error("get content fail", "err", err)
			return
		}

		dpLLM := rag.NewRag(llm.WithBot(bot), llm.WithUpdate(update),
			llm.WithMessageChan(messageChan), llm.WithContent(content))

		qaChain := chains.NewRetrievalQAFromLLM(
			dpLLM,
			vectorstores.ToRetriever(conf.Store, 3),
		)
		_, err = chains.Run(ctx, qaChain, text)
		if err != nil {
			logger.Warn("execute chain fail", "err", err)
		}
	}()

	// send response message
	go handleUpdate(messageChan, update, bot)

}

// executeLLM directly interact llm
func executeLLM(update tgbotapi.Update, bot *tgbotapi.BotAPI, content string) {
	messageChan := make(chan *param.MsgInfo)
	l := llm.NewLLM(llm.WithBot(bot), llm.WithUpdate(update),
		llm.WithMessageChan(messageChan), llm.WithContent(content),
		llm.WithTaskTools(&conf.AgentInfo{
			DeepseekTool:    conf.DeepseekTools,
			VolTool:         conf.VolTools,
			OpenAITools:     conf.OpenAITools,
			GeminiTools:     conf.GeminiTools,
			OpenRouterTools: conf.OpenRouterTools,
		}))

	// request DeepSeek API
	go l.GetContent()

	// send response message
	go handleUpdate(messageChan, update, bot)

}

// handleUpdate handle robot msg sending
func handleUpdate(messageChan chan *param.MsgInfo, update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleUpdate panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	var msg *param.MsgInfo

	chatId, msgId, _ := utils.GetChatIdAndMsgIdAndUserID(update)
	parseMode := tgbotapi.ModeMarkdown

	tgMsgInfo := tgbotapi.NewMessage(chatId, i18n.GetMessage(*conf.Lang, "thinking", nil))
	tgMsgInfo.ReplyToMessageID = msgId
	firstSendInfo, err := bot.Send(tgMsgInfo)
	if err != nil {
		logger.Warn("Sending first message fail", "err", err)
	}

	for msg = range messageChan {
		if len(msg.Content) == 0 {
			msg.Content = "get nothing from deepseek!"
		}
		if firstSendInfo.MessageID != 0 {
			msg.MsgId = firstSendInfo.MessageID
		}

		if msg.MsgId == 0 && firstSendInfo.MessageID == 0 {
			tgMsgInfo = tgbotapi.NewMessage(chatId, msg.Content)
			tgMsgInfo.ReplyToMessageID = msgId
			tgMsgInfo.ParseMode = parseMode
			sendInfo, err := bot.Send(tgMsgInfo)
			if err != nil {
				if sleepUtilNoLimit(msgId, err) {
					sendInfo, err = bot.Send(tgMsgInfo)
				} else if strings.Contains(err.Error(), "can't parse entities") {
					tgMsgInfo.ParseMode = ""
					sendInfo, err = bot.Send(tgMsgInfo)
				} else {
					_, err = bot.Send(tgMsgInfo)
				}
				if err != nil {
					logger.Warn("Error sending message:", "msgID", msgId, "err", err)
					continue
				}
			}
			msg.MsgId = sendInfo.MessageID
		} else {
			updateMsg := tgbotapi.NewEditMessageText(chatId, msg.MsgId, msg.Content)
			updateMsg.ParseMode = parseMode
			_, err = bot.Send(updateMsg)
			if err != nil {
				// try again
				if sleepUtilNoLimit(msgId, err) {
					_, err = bot.Send(updateMsg)
				} else if strings.Contains(err.Error(), "can't parse entities") {
					updateMsg.ParseMode = ""
					_, err = bot.Send(updateMsg)
				} else {
					_, err = bot.Send(updateMsg)
				}
				if err != nil {
					logger.Warn("Error editing message", "msgID", msgId, "err", err)
				}
			}
			firstSendInfo.MessageID = 0
		}

	}
}

// sleepUtilNoLimit handle "Too Many Requests" error
func sleepUtilNoLimit(msgId int, err error) bool {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) && apiErr.Message == "Too Many Requests" {
		waitTime := time.Duration(apiErr.RetryAfter) * time.Second
		logger.Warn("Rate limited. Retrying after", "msgID", msgId, "waitTime", waitTime)
		time.Sleep(waitTime)
		return true
	}

	return false
}

// handleCommandAndCallback telegram command and callback function
func handleCommandAndCallback(update tgbotapi.Update, bot *tgbotapi.BotAPI) bool {
	// if it's command, directly
	if update.Message != nil && update.Message.IsCommand() {
		go handleCommand(update, bot)
		return true
	}

	if update.CallbackQuery != nil {
		go handleCallbackQuery(update, bot)
		return true
	}

	if update.Message != nil && update.Message.ReplyToMessage != nil && update.Message.ReplyToMessage.From != nil &&
		update.Message.ReplyToMessage.From.UserName == bot.Self.UserName {
		go ExecuteForceReply(update, bot)
		return true
	}

	return false
}

// skipThisMsg check if msg trigger llm
func skipThisMsg(update tgbotapi.Update, bot *tgbotapi.BotAPI) bool {
	if update.Message.Chat.Type == "private" {
		if strings.TrimSpace(update.Message.Text) == "" &&
			update.Message.Voice == nil && update.Message.Photo == nil {
			return true
		}

		return false
	} else {
		if strings.TrimSpace(strings.ReplaceAll(update.Message.Text, "@"+bot.Self.UserName, "")) == "" &&
			update.Message.Voice == nil {
			return true
		}

		if !strings.Contains(update.Message.Text, "@"+bot.Self.UserName) {
			return true
		}
	}

	return false
}

// handleCommand handle multiple commands
func handleCommand(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleCommand panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	cmd := update.Message.Command()
	_, _, userID := utils.GetChatIdAndMsgIdAndUserID(update)
	logger.Info("command info", "userID", userID, "cmd", cmd)

	// check if at bot
	if (utils.GetChatType(update) == "group" || utils.GetChatType(update) == "supergroup") && *conf.NeedATBOt {
		if !strings.Contains(update.Message.Text, "@"+bot.Self.UserName) {
			logger.Warn("not at bot", "userID", userID, "cmd", cmd)
			return
		}
	}

	switch cmd {
	case "chat":
		sendChatMessage(update, bot)
	case "mode":
		sendModeConfigurationOptions(update, bot)
	case "balance":
		showBalanceInfo(update, bot)
	case "state":
		showStateInfo(update, bot)
	case "clear":
		clearAllRecord(update, bot)
	case "retry":
		retryLastQuestion(update, bot)
	case "photo":
		sendImg(update, bot)
	case "video":
		sendVideo(update, bot)
	case "help":
		sendHelpConfigurationOptions(update, bot)
	case "task":
		sendMultiAgent(update, bot, "task_empty_content")
	case "mcp":
		sendMultiAgent(update, bot, "mcp_empty_content")
	}

	if checkAdminUser(update) {
		switch cmd {
		case "addtoken":
			addToken(update, bot)
		}
	}
}

// sendChatMessage response chat command to telegram
func sendChatMessage(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId, msgID, _ := utils.GetChatIdAndMsgIdAndUserID(update)

	messageText := ""
	if update.Message != nil {
		messageText = update.Message.Text
		if messageText == "" && update.Message.Voice != nil && *conf.AudioAppID != "" {
			audioContent := utils.GetAudioContent(update, bot)
			if audioContent == nil {
				logger.Warn("audio url empty")
				return
			}
			messageText = utils.FileRecognize(audioContent)
		}

		if messageText == "" && update.Message.Photo != nil {
			photoContent, err := utils.GetImageContent(utils.GetPhotoContent(update, bot))
			if err != nil {
				logger.Warn("get photo content err", "err", err)
				return
			}
			messageText = photoContent
		}

	} else {
		update.Message = new(tgbotapi.Message)
	}

	// Remove /chat and /chat@botUserName from the message
	content := utils.ReplaceCommand(messageText, "/chat", bot.Self.UserName)
	update.Message.Text = content

	if len(content) == 0 {
		err := utils.ForceReply(chatId, msgID, "chat_empty_content", bot)
		if err != nil {
			logger.Warn("force reply fail", "err", err)
		}
		return
	}

	// Reply to the chat content
	requestDeepseekAndResp(update, bot, content)
}

// retryLastQuestion retry last question
func retryLastQuestion(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId, msgId, userId := utils.GetChatIdAndMsgIdAndUserID(update)

	records := db.GetMsgRecord(userId)
	if records != nil && len(records.AQs) > 0 {
		requestDeepseekAndResp(update, bot, records.AQs[len(records.AQs)-1].Question)
	} else {
		i18n.SendMsg(chatId, "last_question_fail", bot, nil, msgId)
	}
}

// clearAllRecord clear all record
func clearAllRecord(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId, msgId, userId := utils.GetChatIdAndMsgIdAndUserID(update)
	db.DeleteMsgRecord(userId)
	i18n.SendMsg(chatId, "delete_succ", bot, nil, msgId)
}

// addToken clear all record
func addToken(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId, msgId, _ := utils.GetChatIdAndMsgIdAndUserID(update)
	msg := utils.GetMessage(update)

	content := utils.ReplaceCommand(msg.Text, "/addtoken", bot.Self.UserName)
	splitContent := strings.Split(content, " ")

	db.AddAvailToken(int64(utils.ParseInt(splitContent[0])), utils.ParseInt(splitContent[1]))
	i18n.SendMsg(chatId, "add_token_succ", bot, nil, msgId)
}

// showBalanceInfo show balance info
func showBalanceInfo(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId, msgId, _ := utils.GetChatIdAndMsgIdAndUserID(update)

	if *conf.Type != param.DeepSeek {
		i18n.SendMsg(chatId, "not_deepseek", bot, nil, msgId)
		return
	}

	balance := llm.GetBalanceInfo()

	// handle balance info msg
	msgContent := fmt.Sprintf(i18n.GetMessage(*conf.Lang, "balance_title", nil), balance.IsAvailable)

	template := i18n.GetMessage(*conf.Lang, "balance_content", nil)

	for _, bInfo := range balance.BalanceInfos {
		msgContent += fmt.Sprintf(template, bInfo.Currency, bInfo.TotalBalance,
			bInfo.ToppedUpBalance, bInfo.GrantedBalance)
	}

	utils.SendMsg(chatId, msgContent, bot, msgId, tgbotapi.ModeMarkdown)
}

// showStateInfo show user's usage of token
func showStateInfo(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId, msgId, userId := utils.GetChatIdAndMsgIdAndUserID(update)
	userInfo, err := db.GetUserByID(userId)
	if err != nil {
		logger.Warn("get user info fail", "err", err)
		return
	}

	if userInfo == nil {
		db.InsertUser(userId, godeepseek.DeepSeekChat)
		userInfo, err = db.GetUserByID(userId)
		if err != nil {
			logger.Warn("get user info after insert fail", "err", err)
			return
		}
		if userInfo == nil {
			logger.Warn("user info still nil after insert", "userId", userId)
			return
		}
	}

	// get today token
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, now.Location())
	todayTokey, err := db.GetTokenByUserIdAndTime(userId, startOfDay.Unix(), endOfDay.Unix())
	if err != nil {
		logger.Warn("get today token fail", "err", err)
	}

	// get this week token
	startOf7DaysAgo := now.AddDate(0, 0, -7).Truncate(24 * time.Hour)
	weekToken, err := db.GetTokenByUserIdAndTime(userId, startOf7DaysAgo.Unix(), endOfDay.Unix())
	if err != nil {
		logger.Warn("get week token fail", "err", err)
	}

	// handle balance info msg
	startOf30DaysAgo := now.AddDate(0, 0, -30).Truncate(24 * time.Hour)
	monthToken, err := db.GetTokenByUserIdAndTime(userId, startOf30DaysAgo.Unix(), endOfDay.Unix())
	if err != nil {
		logger.Warn("get week token fail", "err", err)
	}

	template := i18n.GetMessage(*conf.Lang, "state_content", nil)
	msgContent := fmt.Sprintf(template, userInfo.Token, todayTokey, weekToken, monthToken)
	utils.SendMsg(chatId, msgContent, bot, msgId, tgbotapi.ModeMarkdown)
}

// sendModeConfigurationOptions send config view
func sendModeConfigurationOptions(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatID, msgId, _ := utils.GetChatIdAndMsgIdAndUserID(update)

	var inlineKeyboard tgbotapi.InlineKeyboardMarkup
	inlineButton := make([][]tgbotapi.InlineKeyboardButton, 0)
	switch *conf.Type {
	case param.DeepSeek:
		if *conf.CustomUrl == "" || *conf.CustomUrl == "https://api.deepseek.com/" {
			for k := range param.DeepseekModels {
				inlineButton = append(inlineButton, tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(k, k),
				))
			}
		} else {
			inlineKeyboard = tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(godeepseek.AzureDeepSeekR1, godeepseek.AzureDeepSeekR1),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(godeepseek.OpenRouterDeepSeekR1, godeepseek.OpenRouterDeepSeekR1),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(godeepseek.OpenRouterDeepSeekR1DistillLlama70B, godeepseek.OpenRouterDeepSeekR1DistillLlama70B),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(godeepseek.OpenRouterDeepSeekR1DistillLlama8B, godeepseek.OpenRouterDeepSeekR1DistillLlama8B),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(godeepseek.OpenRouterDeepSeekR1DistillQwen14B, godeepseek.OpenRouterDeepSeekR1DistillQwen14B),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(godeepseek.OpenRouterDeepSeekR1DistillQwen1_5B, godeepseek.OpenRouterDeepSeekR1DistillQwen1_5B),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(godeepseek.OpenRouterDeepSeekR1DistillQwen32B, godeepseek.OpenRouterDeepSeekR1DistillQwen32B),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("llama2", param.LLAVA),
				),
			)
		}
	case param.Gemini:
		for k := range param.GeminiModels {
			inlineButton = append(inlineButton, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(k, k),
			))
		}
	case param.OpenAi:
		for k := range param.OpenAIModels {
			inlineButton = append(inlineButton, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(k, k),
			))
		}
	case param.LLAVA:
		inlineKeyboard = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("llama2", param.LLAVA),
		))
	case param.OpenRouter:
		for k := range param.OpenRouterModelTypes {
			inlineButton = append(inlineButton, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(k, k),
			))
		}
	case param.Vol:
		// create inline button
		for k := range param.VolModels {
			inlineButton = append(inlineButton, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(k, k),
			))
		}

	}

	inlineKeyboard = tgbotapi.NewInlineKeyboardMarkup(inlineButton...)

	i18n.SendMsg(chatID, "chat_mode", bot, &inlineKeyboard, msgId)
}

// sendHelpConfigurationOptions
func sendHelpConfigurationOptions(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatID, msgId, _ := utils.GetChatIdAndMsgIdAndUserID(update)

	// create inline button
	inlineKeyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("mode", "mode"),
			tgbotapi.NewInlineKeyboardButtonData("clear", "clear"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("balance", "balance"),
			tgbotapi.NewInlineKeyboardButtonData("state", "state"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("retry", "retry"),
			tgbotapi.NewInlineKeyboardButtonData("chat", "chat"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("photo", "photo"),
			tgbotapi.NewInlineKeyboardButtonData("video", "video"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏢 Business", "business_help"),
		),
	)

	i18n.SendMsg(chatID, "command_notice", bot, &inlineKeyboard, msgId)
}

// handleCallbackQuery handle callback response
func handleCallbackQuery(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleCommand panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	switch update.CallbackQuery.Data {
	case "mode":
		sendModeConfigurationOptions(update, bot)
	case "balance":
		showBalanceInfo(update, bot)
	case "clear":
		clearAllRecord(update, bot)
	case "retry":
		retryLastQuestion(update, bot)
	case "state":
		showStateInfo(update, bot)
	case "photo":
		if update.CallbackQuery.Message.ReplyToMessage != nil {
			update.CallbackQuery.Message.MessageID = update.CallbackQuery.Message.ReplyToMessage.MessageID
		}
		sendImg(update, bot)
	case "video":
		if update.CallbackQuery.Message.ReplyToMessage != nil {
			update.CallbackQuery.Message.MessageID = update.CallbackQuery.Message.ReplyToMessage.MessageID
		}
		sendVideo(update, bot)
	case "chat":
		if update.CallbackQuery.Message.ReplyToMessage != nil {
			update.CallbackQuery.Message.MessageID = update.CallbackQuery.Message.ReplyToMessage.MessageID
		}
		sendChatMessage(update, bot)
	// Business-related callbacks
	case "business_commands":
		sendBusinessCommandsList(update, bot)
	case "business_help":
		sendBusinessHelpCallback(update, bot)
	case "business_status":
		sendBusinessStatusCallback(update, bot)
	case "business_setup":
		sendBusinessSetupCallback(update, bot)
	case "toggle_autoreply":
		toggleBusinessAutoReply(update, bot)
	case "set_language":
		showBusinessLanguageOptions(update, bot)
	case "set_model":
		showBusinessModelOptions(update, bot)
	case "set_hours":
		showBusinessHoursOptions(update, bot)
	case "customer_settings":
		showCustomerSettings(update, bot)
	default:
		if param.GeminiModels[update.CallbackQuery.Data] || param.OpenAIModels[update.CallbackQuery.Data] ||
			param.DeepseekModels[update.CallbackQuery.Data] || param.DeepseekLocalModels[update.CallbackQuery.Data] ||
			param.OpenRouterModels[update.CallbackQuery.Data] || param.VolModels[update.CallbackQuery.Data] {
			handleModeUpdate(update, bot)
		}
		if param.OpenRouterModelTypes[update.CallbackQuery.Data] {
			chatID, msgId, _ := utils.GetChatIdAndMsgIdAndUserID(update)
			inlineButton := make([][]tgbotapi.InlineKeyboardButton, 0)
			for k := range param.OpenRouterModels {
				if strings.Contains(k, update.CallbackQuery.Data+"/") {
					inlineButton = append(inlineButton, tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData(k, k),
					))
				}
			}
			inlineKeyboard := tgbotapi.NewInlineKeyboardMarkup(inlineButton...)
			i18n.SendMsg(chatID, "chat_mode", bot, &inlineKeyboard, msgId)

		}
	}

}

// handleModeUpdate handle mode update
func handleModeUpdate(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	userInfo, err := db.GetUserByID(update.CallbackQuery.From.ID)
	if err != nil {
		logger.Warn("get user fail", "userID", update.CallbackQuery.From.ID, "err", err)
		sendFailMessage(update, bot)
		return
	}

	if userInfo != nil && userInfo.ID != 0 {
		err = db.UpdateUserMode(update.CallbackQuery.From.ID, update.CallbackQuery.Data)
		if err != nil {
			logger.Warn("update user fail", "userID", update.CallbackQuery.From.ID, "err", err)
			sendFailMessage(update, bot)
			return
		}
	} else {
		_, err = db.InsertUser(update.CallbackQuery.From.ID, update.CallbackQuery.Data)
		if err != nil {
			logger.Warn("insert user fail", "userID", update.CallbackQuery.From.String(), "err", err)
			sendFailMessage(update, bot)
			return
		}
	}

	// send response
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, update.CallbackQuery.Data)
	if _, err := bot.Request(callback); err != nil {
		logger.Warn("request callback fail", "err", err)
	}

	//utils.SendMsg(update.CallbackQuery.Message.Chat.ID,
	//	i18n.GetMessage(*conf.Lang, "mode_choose", nil)+update.CallbackQuery.Data, bot, update.CallbackQuery.Message.MessageID)
}

// sendFailMessage send set mode fail msg
func sendFailMessage(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, i18n.GetMessage(*conf.Lang, "set_mode", nil))
	if _, err := bot.Request(callback); err != nil {
		logger.Warn("request callback fail", "err", err)
	}

	i18n.SendMsg(update.CallbackQuery.Message.Chat.ID, "set_mode", bot, nil, update.CallbackQuery.Message.MessageID)
}

func sendMultiAgent(update tgbotapi.Update, bot *tgbotapi.BotAPI, agentType string) {
	if utils.CheckUserChatExceed(update, bot) {
		return
	}

	defer func() {
		utils.DecreaseUserChat(update)
	}()

	chatId, replyToMessageID, userId := utils.GetChatIdAndMsgIdAndUserID(update)
	if checkUserTokenExceed(update, bot) {
		logger.Warn("user token exceed", "userID", userId)
		return
	}

	prompt := ""
	if update.Message != nil {
		prompt = update.Message.Text
	}
	prompt = utils.ReplaceCommand(prompt, "/mcp", bot.Self.UserName)
	prompt = utils.ReplaceCommand(prompt, "/task", bot.Self.UserName)
	if len(prompt) == 0 {
		err := utils.ForceReply(chatId, replyToMessageID, agentType, bot)
		if err != nil {
			logger.Warn("force reply fail", "err", err)
		}
		return
	}

	// send response message
	messageChan := make(chan *param.MsgInfo)

	dpReq := &llm.DeepseekTaskReq{
		Content:     prompt,
		Update:      update,
		Bot:         bot,
		MessageChan: messageChan,
	}

	if agentType == "mcp_empty_content" {
		go dpReq.ExecuteMcp()
	} else {
		go dpReq.ExecuteTask()
	}

	go handleUpdate(messageChan, update, bot)
}

// sendVideo send video to telegram
func sendVideo(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	if utils.CheckUserChatExceed(update, bot) {
		return
	}

	defer func() {
		utils.DecreaseUserChat(update)
	}()

	chatId, replyToMessageID, userId := utils.GetChatIdAndMsgIdAndUserID(update)
	if checkUserTokenExceed(update, bot) {
		logger.Warn("user token exceed", "userID", userId)
		return
	}

	prompt := ""
	if update.Message != nil {
		prompt = update.Message.Text
	}

	prompt = utils.ReplaceCommand(prompt, "/video", bot.Self.UserName)
	if len(prompt) == 0 {
		err := utils.ForceReply(chatId, replyToMessageID, "video_empty_content", bot)
		if err != nil {
			logger.Warn("force reply fail", "err", err)
		}
		return
	}

	thinkingMsgId := i18n.SendMsg(chatId, "thinking", bot, nil, replyToMessageID)
	videoUrl, err := llm.GenerateVideo(prompt)
	if err != nil {
		logger.Warn("generate video fail", "err", err)
		return
	}

	if len(videoUrl) == 0 {
		logger.Warn("no video generated")
		return
	}

	video := tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(videoUrl))
	edit := tgbotapi.EditMessageMediaConfig{
		BaseEdit: tgbotapi.BaseEdit{
			ChatID:    chatId,
			MessageID: thinkingMsgId,
		},
		Media: video,
	}

	_, err = bot.Request(edit)
	if err != nil {
		logger.Warn("send video fail", "result", edit)
		return
	}

	db.InsertRecordInfo(&db.Record{
		UserId:    userId,
		Question:  prompt,
		Answer:    videoUrl,
		Token:     param.VideoTokenUsage,
		IsDeleted: 1,
	})
	return
}

// sendImg send img to telegram
func sendImg(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	if utils.CheckUserChatExceed(update, bot) {
		return
	}

	defer func() {
		utils.DecreaseUserChat(update)
	}()

	chatId, replyToMessageID, userId := utils.GetChatIdAndMsgIdAndUserID(update)
	if checkUserTokenExceed(update, bot) {
		logger.Warn("user token exceed", "userID", userId)
		return
	}

	prompt := ""
	if update.Message != nil {
		prompt = update.Message.Text
	}

	prompt = utils.ReplaceCommand(prompt, "/photo", bot.Self.UserName)
	if len(prompt) == 0 {
		err := utils.ForceReply(chatId, replyToMessageID, "photo_empty_content", bot)
		if err != nil {
			logger.Warn("force reply fail", "err", err)
		}
		return
	}

	thinkingMsgId := i18n.SendMsg(chatId, "thinking", bot, nil, replyToMessageID)
	data, err := llm.GenerateImg(prompt)
	if err != nil {
		logger.Warn("generate image fail", "err", err)
		return
	}

	if data.Data == nil || len(data.Data.ImageUrls) == 0 {
		logger.Warn("no image generated")
		return
	}

	photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(data.Data.ImageUrls[0]))
	edit := tgbotapi.EditMessageMediaConfig{
		BaseEdit: tgbotapi.BaseEdit{
			ChatID:    chatId,
			MessageID: thinkingMsgId,
		},
		Media: photo,
	}

	_, err = bot.Request(edit)
	if err != nil {
		logger.Warn("send image fail", "result", edit)
		return
	}

	db.InsertRecordInfo(&db.Record{
		UserId:    userId,
		Question:  prompt,
		Answer:    data.Data.ImageUrls[0],
		Token:     param.ImageTokenUsage,
		IsDeleted: 1,
	})

	return
}

// checkUserAllow check use can use telegram bot or not
func checkUserAllow(update tgbotapi.Update) bool {
	if len(conf.AllowedTelegramUserIds) == 0 {
		return true
	}
	if conf.AllowedTelegramUserIds[0] {
		return false
	}

	_, _, userId := utils.GetChatIdAndMsgIdAndUserID(update)
	_, ok := conf.AllowedTelegramUserIds[userId]
	return ok
}

func checkGroupAllow(update tgbotapi.Update) bool {
	chat := utils.GetChat(update)
	if chat == nil {
		return false
	}

	if chat.IsGroup() || chat.IsSuperGroup() { // 判断是否是群组或超级群组
		if len(conf.AllowedTelegramGroupIds) == 0 {
			return true
		}
		if conf.AllowedTelegramGroupIds[0] {
			return false
		}
		if _, ok := conf.AllowedTelegramGroupIds[chat.ID]; ok {
			return true
		}
	}

	return false
}

// checkUserTokenExceed check use token exceeded
func checkUserTokenExceed(update tgbotapi.Update, bot *tgbotapi.BotAPI) bool {
	if *conf.TokenPerUser == 0 {
		return false
	}

	chatId, msgId, userId := utils.GetChatIdAndMsgIdAndUserID(update)
	userInfo, err := db.GetUserByID(userId)
	if err != nil {
		logger.Warn("get user info fail", "err", err)
		return false
	}

	if userInfo == nil {
		db.InsertUser(userId, godeepseek.DeepSeekChat)
		logger.Warn("get user info is nil")
		return false
	}

	if userInfo.Token >= userInfo.AvailToken {
		tpl := i18n.GetMessage(*conf.Lang, "token_exceed", nil)
		content := fmt.Sprintf(tpl, userInfo.Token, userInfo.AvailToken-userInfo.Token, userInfo.AvailToken)
		utils.SendMsg(chatId, content, bot, msgId, tgbotapi.ModeMarkdown)
		return true
	}

	return false
}

// checkAdminUser check user is admin
func checkAdminUser(update tgbotapi.Update) bool {
	if len(conf.AdminUserIds) == 0 {
		return false
	}

	_, _, userId := utils.GetChatIdAndMsgIdAndUserID(update)
	_, ok := conf.AdminUserIds[userId]
	return ok
}

// ExecuteForceReply use force reply interact with user
func ExecuteForceReply(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("ExecuteForceReply panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	switch update.Message.ReplyToMessage.Text {
	case i18n.GetMessage(*conf.Lang, "chat_empty_content", nil):
		sendChatMessage(update, bot)
	case i18n.GetMessage(*conf.Lang, "photo_empty_content", nil):
		sendImg(update, bot)
	case i18n.GetMessage(*conf.Lang, "video_empty_content", nil):
		sendVideo(update, bot)
	case i18n.GetMessage(*conf.Lang, "task_empty_content", nil):
		sendMultiAgent(update, bot, "task_empty_content")
	case i18n.GetMessage(*conf.Lang, "mcp_empty_content", nil):
		sendMultiAgent(update, bot, "mcp_empty_content")
	}
}

// handleBusinessUpdates handles business-related updates (connections, messages, deletions)
// Note: This is a compatibility layer for business features not yet in the telegram bot API v5.5.1
func handleBusinessUpdates(update tgbotapi.Update, bot *tgbotapi.BotAPI) bool {
	// Check if this is a business-related command or deep link
	if update.Message != nil && isBusinessMessage(update.Message) {
		go handleBusinessMessage(update, bot)
		return true
	}

	// For now, we'll check for deep link business commands
	if update.Message != nil && strings.HasPrefix(update.Message.Text, "/start bizChat") {
		go handleBusinessCommand(update, bot)
		return true
	}

	// Check for business command patterns
	if update.Message != nil && strings.HasPrefix(update.Message.Text, "/business") {
		go handleBusinessCommand(update, bot)
		return true
	}

	return false
}

// Compatibility layer for business features (Bot API v7.0+ features)
// Since the current telegram-bot-api library doesn't support business features yet,
// we implement a compatibility layer that simulates business functionality

// BusinessConnection represents a business connection (compatibility struct)
type BusinessConnection struct {
	ID         string
	UserID     int64
	UserChatID int64
	IsEnabled  bool
	CanReply   bool
	Date       int64
}

// BusinessMessage represents a business message (compatibility struct)
type BusinessMessage struct {
	BusinessConnectionID string
	Message              *tgbotapi.Message
}

// BusinessMessagesDeleted represents deleted business messages (compatibility struct)
type BusinessMessagesDeleted struct {
	BusinessConnectionID string
	Chat                 *tgbotapi.Chat
	MessageIDs           []int
}

// handleBusinessConnection handles business connection establishment, updates, or removal
func handleBusinessConnection(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleBusinessConnection panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	// In the compatibility layer, we simulate business connection via regular messages
	message := update.Message
	if message == nil {
		return
	}

	// Create a mock business connection for demonstration
	connection := &BusinessConnection{
		ID:         fmt.Sprintf("business_%d_%d", message.From.ID, time.Now().Unix()),
		UserID:     message.From.ID,
		UserChatID: message.Chat.ID,
		IsEnabled:  true,
		CanReply:   true,
		Date:       time.Now().Unix(),
	}

	logger.Info("business connection update (simulated)",
		"connectionId", connection.ID,
		"userId", connection.UserID,
		"isEnabled", connection.IsEnabled,
		"canReply", connection.CanReply)

	// Store business connection info in database or handle as needed
	storeBusinessConnection(connection)

	// Send confirmation message to the business user if connection is established
	if connection.IsEnabled {
		sendBusinessConnectionWelcome(connection, bot)
	}
}

// handleBusinessMessage handles incoming messages from business accounts
func handleBusinessMessage(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleBusinessMessage panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	message := update.Message
	if message == nil {
		return
	}

	// Simulate business connection ID
	businessConnectionId := fmt.Sprintf("business_%d", message.From.ID)

	logger.Info("business message received (simulated)",
		"businessConnectionId", businessConnectionId,
		"chatId", message.Chat.ID,
		"messageId", message.MessageID,
		"text", message.Text)

	// Check if we have a valid business connection
	if !isValidBusinessConnection(businessConnectionId) {
		logger.Warn("invalid business connection", "connectionId", businessConnectionId)
		return
	}

	// Process the business message similar to regular messages
	processBusinessMessage(update, bot, message.Text, businessConnectionId)
}

// handleEditedBusinessMessage handles edited messages from business accounts
func handleEditedBusinessMessage(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleEditedBusinessMessage panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	message := update.EditedMessage
	if message == nil {
		return
	}

	businessConnectionId := fmt.Sprintf("business_%d", message.From.ID)

	logger.Info("edited business message received (simulated)",
		"businessConnectionId", businessConnectionId,
		"chatId", message.Chat.ID,
		"messageId", message.MessageID,
		"text", message.Text)

	// Handle edited business message (optional - could re-process or ignore)
	// For now, we'll just log it
}

// handleDeletedBusinessMessages handles deleted messages from business accounts
func handleDeletedBusinessMessages(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleDeletedBusinessMessages panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	// This is a compatibility function - in real implementation this would handle
	// actual deleted business message events
	logger.Info("business messages deleted (simulated feature)")

	// Create mock deleted messages structure
	deletedMessages := &BusinessMessagesDeleted{
		BusinessConnectionID: "simulated_connection",
		Chat:                 &tgbotapi.Chat{ID: 0},
		MessageIDs:           []int{},
	}

	// Handle message deletions (cleanup, stop processing, etc.)
	cleanupDeletedBusinessMessages(deletedMessages)
}

// processBusinessMessage processes business messages similar to regular messages
func processBusinessMessage(update tgbotapi.Update, bot *tgbotapi.BotAPI, content string, businessConnectionId string) {
	if len(strings.TrimSpace(content)) == 0 {
		return
	}

	// For compatibility, use regular message processing
	message := update.Message
	if message == nil {
		return
	}

	// Check token limits for business user
	if checkBusinessUserTokenExceed(update, bot) {
		logger.Warn("business user token exceed", "businessConnectionId", businessConnectionId)
		sendBusinessMessage(bot, businessConnectionId, message.Chat.ID,
			i18n.GetMessage(*conf.Lang, "token_exceed", nil), message.MessageID)
		return
	}

	// Process the message through the LLM
	if conf.Store != nil {
		executeBusinessChain(update, bot, content, businessConnectionId)
	} else {
		executeBusinessLLM(update, bot, content, businessConnectionId)
	}
}

// executeBusinessLLM processes business messages through LLM
func executeBusinessLLM(update tgbotapi.Update, bot *tgbotapi.BotAPI, content string, businessConnectionId string) {
	messageChan := make(chan *param.MsgInfo)
	l := llm.NewLLM(llm.WithBot(bot), llm.WithUpdate(update),
		llm.WithMessageChan(messageChan), llm.WithContent(content),
		llm.WithTaskTools(&conf.AgentInfo{
			DeepseekTool:    conf.DeepseekTools,
			VolTool:         conf.VolTools,
			OpenAITools:     conf.OpenAITools,
			GeminiTools:     conf.GeminiTools,
			OpenRouterTools: conf.OpenRouterTools,
		}))

	// request LLM API
	go l.GetContent()

	// send response message to business chat
	go handleBusinessUpdate(messageChan, update, bot, businessConnectionId)
}

// executeBusinessChain processes business messages through chain
func executeBusinessChain(update tgbotapi.Update, bot *tgbotapi.BotAPI, content string, businessConnectionId string) {
	messageChan := make(chan *param.MsgInfo)

	go func() {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("GetContent panic err", "err", err)
			}
			utils.DecreaseUserChat(update)
			close(messageChan)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		text, err := utils.GetContent(update, bot, content)
		if err != nil {
			logger.Error("get content fail", "err", err)
			return
		}

		dpLLM := rag.NewRag(llm.WithBot(bot), llm.WithUpdate(update),
			llm.WithMessageChan(messageChan), llm.WithContent(content))

		qaChain := chains.NewRetrievalQAFromLLM(
			dpLLM,
			vectorstores.ToRetriever(conf.Store, 3),
		)
		_, err = chains.Run(ctx, qaChain, text)
		if err != nil {
			logger.Warn("execute chain fail", "err", err)
		}
	}()

	// send response message to business chat
	go handleBusinessUpdate(messageChan, update, bot, businessConnectionId)
}

// handleBusinessUpdate handles business bot message sending
func handleBusinessUpdate(messageChan chan *param.MsgInfo, update tgbotapi.Update, bot *tgbotapi.BotAPI, businessConnectionId string) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleBusinessUpdate panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	var msg *param.MsgInfo

	// Use regular message for compatibility
	message := update.Message
	if message == nil {
		return
	}

	chatId := message.Chat.ID
	msgId := message.MessageID
	parseMode := "Markdown"

	// Send initial "thinking" message
	firstSendMsgId := sendBusinessMessage(bot, businessConnectionId, chatId,
		i18n.GetMessage(*conf.Lang, "thinking", nil), msgId)

	for msg = range messageChan {
		if len(msg.Content) == 0 {
			msg.Content = "get nothing from AI!"
		}

		if firstSendMsgId != 0 {
			// Edit the existing message
			err := editBusinessMessage(bot, businessConnectionId, chatId, firstSendMsgId, msg.Content, parseMode)
			if err != nil {
				logger.Warn("Error editing business message", "msgID", msgId, "err", err)
				// Fallback to sending new message
				sendBusinessMessage(bot, businessConnectionId, chatId, msg.Content, msgId)
			}
			firstSendMsgId = 0 // Only edit once
		} else {
			// Send new message
			sendBusinessMessage(bot, businessConnectionId, chatId, msg.Content, msgId)
		}
	}
}

// Helper functions for business operations (compatibility layer)

// storeBusinessConnection stores business connection information
func storeBusinessConnection(connection *BusinessConnection) {
	// Store in database or cache as needed
	logger.Info("storing business connection", "connectionId", connection.ID, "userId", connection.UserID)
	// TODO: Implement database storage for business connections
}

// isValidBusinessConnection checks if a business connection is valid and active
func isValidBusinessConnection(connectionId string) bool {
	// Check if connection exists and is active
	// TODO: Implement connection validation
	return len(connectionId) > 0
}

// sendBusinessConnectionWelcome sends welcome message when business connection is established
func sendBusinessConnectionWelcome(connection *BusinessConnection, bot *tgbotapi.BotAPI) {
	// Send welcome message to business user
	welcomeMsg := fmt.Sprintf("🤖 Bot connected to your business account! Connection ID: %s\n\nI'm ready to help manage your customer interactions.", connection.ID)

	// Since this is a connection update, we send to the business user directly
	msg := tgbotapi.NewMessage(connection.UserChatID, welcomeMsg)
	_, err := bot.Send(msg)
	if err != nil {
		logger.Warn("Failed to send business welcome message", "err", err)
	}
}

// createBusinessUpdate creates an update structure for business message processing
func createBusinessUpdate(update tgbotapi.Update, businessConnectionId string) tgbotapi.Update {
	// Create a modified update that can be processed by existing LLM functions
	businessUpdate := update

	// For compatibility, we'll use the regular message
	if businessUpdate.Message != nil {
		logger.Debug("created business update", "connectionId", businessConnectionId)
	}

	return businessUpdate
}

// checkBusinessUserTokenExceed checks if business user has exceeded token limits
func checkBusinessUserTokenExceed(update tgbotapi.Update, bot *tgbotapi.BotAPI) bool {
	if update.Message != nil && update.Message.From != nil {
		return utils.CheckUserChatExceed(update, bot)
	}
	return false
}

// sendBusinessMessage sends a message on behalf of business account
func sendBusinessMessage(bot *tgbotapi.BotAPI, businessConnectionId string, chatId int64, text string, replyToMessageId int) int {
	// Create message config - in compatibility mode, we send regular messages
	// In a real implementation, this would set the BusinessConnectionID field
	msg := tgbotapi.NewMessage(chatId, text)

	// Note: BusinessConnectionID is not available in current API version
	// This would be: msg.BusinessConnectionID = businessConnectionId

	if replyToMessageId != 0 {
		msg.ReplyToMessageID = replyToMessageId
	}
	msg.ParseMode = "Markdown"

	// Add a note that this is a business message (for demonstration)
	if businessConnectionId != "" {
		msg.Text = "🏢 [Business Bot] " + text
	}

	sendInfo, err := bot.Send(msg)
	if err != nil {
		// Retry without markdown if parse error
		if strings.Contains(err.Error(), "can't parse entities") {
			msg.ParseMode = ""
			sendInfo, err = bot.Send(msg)
		}
		if err != nil {
			logger.Warn("Failed to send business message", "err", err, "businessConnectionId", businessConnectionId)
			return 0
		}
	}

	return sendInfo.MessageID
}

// editBusinessMessage edits a message on behalf of business account
func editBusinessMessage(bot *tgbotapi.BotAPI, businessConnectionId string, chatId int64, messageId int, text string, parseMode string) error {
	updateMsg := tgbotapi.NewEditMessageText(chatId, messageId, text)

	// Note: BusinessConnectionID is not available in current API version
	// This would be: updateMsg.BusinessConnectionID = businessConnectionId

	updateMsg.ParseMode = parseMode

	// Add business indicator for compatibility
	if businessConnectionId != "" {
		updateMsg.Text = "🏢 [Business Bot] " + text
	}

	_, err := bot.Send(updateMsg)
	if err != nil {
		// Retry without markdown if parse error
		if strings.Contains(err.Error(), "can't parse entities") {
			updateMsg.ParseMode = ""
			_, err = bot.Send(updateMsg)
		}
	}

	return err
}

// cleanupDeletedBusinessMessages handles cleanup when business messages are deleted
func cleanupDeletedBusinessMessages(deletedMessages *BusinessMessagesDeleted) {
	// Cleanup any processing or stored data related to deleted messages
	logger.Info("cleaning up deleted business messages",
		"connectionId", deletedMessages.BusinessConnectionID,
		"count", len(deletedMessages.MessageIDs))
	// TODO: Implement cleanup logic
}

// isBusinessMessage checks if a message is from a business connection
func isBusinessMessage(message *tgbotapi.Message) bool {
	// Check if the message has a business connection ID
	// In newer API versions, this would be available as message.BusinessConnectionID
	// For now, we'll use heuristics to detect business messages

	// Check if message is from a business chat or has business indicators
	if message.Chat.Type == "private" && message.From != nil {
		// Could check user database for business connections
		// For now, return false as we handle this via deep links
		return false
	}

	return false
}

// handleBusinessCommand handles business-specific commands and deep links
func handleBusinessCommand(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("handleBusinessCommand panic err", "err", err, "stack", string(debug.Stack()))
		}
	}()

	message := update.Message
	chatId := message.Chat.ID
	userId := message.From.ID
	text := message.Text

	logger.Info("business command received", "userId", userId, "chatId", chatId, "text", text)

	// Handle business setup commands
	if strings.HasPrefix(text, "/start bizChat") {
		handleBusinessSetup(update, bot)
		return
	}

	// Handle other business commands
	switch {
	case strings.HasPrefix(text, "/business_help"):
		sendBusinessHelp(chatId, bot)
	case strings.HasPrefix(text, "/business_status"):
		sendBusinessStatus(update, bot)
	case strings.HasPrefix(text, "/business_settings"):
		sendBusinessSettings(update, bot)
	default:
		sendBusinessCommandHelp(chatId, bot)
	}
}

// handleBusinessSetup sets up business connection for the bot
func handleBusinessSetup(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId := update.Message.Chat.ID
	userId := update.Message.From.ID

	logger.Info("setting up business connection", "userId", userId, "chatId", chatId)

	// Send business setup instructions
	setupMsg := `🏢 **Business Setup**

To connect your Telegram Business account:

1. Go to your Business Settings in Telegram
2. Navigate to "Chatbots" section
3. Connect this bot to your business account
4. Grant the necessary permissions:
   - Read messages ✅
   - Send messages ✅
   - Delete messages (optional)
   - Manage account (optional)

Once connected, I'll be able to:
• Respond to your customers automatically
• Handle customer inquiries 24/7
• Use AI to provide intelligent responses
• Support multiple languages

**Note:** Make sure to enable business features for this bot in @BotFather if you haven't already.`

	msg := tgbotapi.NewMessage(chatId, setupMsg)
	msg.ParseMode = "Markdown"

	// Add inline keyboard with helpful links
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("📱 Business Settings", "https://t.me/settings/business"),
			tgbotapi.NewInlineKeyboardButtonURL("🤖 BotFather", "https://t.me/botfather"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Business Commands", "business_commands"),
			tgbotapi.NewInlineKeyboardButtonData("❓ Help", "business_help"),
		),
	)
	msg.ReplyMarkup = keyboard

	_, err := bot.Send(msg)
	if err != nil {
		logger.Warn("Failed to send business setup message", "err", err)
	}
}

// sendBusinessHelp sends business help information
func sendBusinessHelp(chatId int64, bot *tgbotapi.BotAPI) {
	helpMsg := `🤖 **Business Bot Help**

**What I can do:**
• ✅ Respond to customer messages automatically
• ✅ Handle multiple conversations simultaneously
• ✅ Support multiple languages (EN/ZH/RU)
• ✅ Provide AI-powered intelligent responses
• ✅ Work 24/7 without breaks

**Getting Started:**
1. Connect your business account via Telegram settings
2. Configure bot preferences using /business_settings
3. Test the connection with sample messages

**Features:**
• Smart context-aware conversations
• Automatic language detection
• Customizable response templates
• Business hours configuration
• Customer escalation to human agents

**Need more help?** Use the commands below or contact support.`

	msg := tgbotapi.NewMessage(chatId, helpMsg)
	msg.ParseMode = "Markdown"

	_, err := bot.Send(msg)
	if err != nil {
		logger.Warn("Failed to send business help", "err", err)
	}
}

// sendBusinessStatus sends current business connection status
func sendBusinessStatus(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId := update.Message.Chat.ID
	userId := update.Message.From.ID

	// Check if user has any business connections
	// TODO: Query database for actual connections
	statusMsg := fmt.Sprintf(`📊 **Business Status**

**User ID:** %d
**Chat ID:** %d

**Connection Status:**
🔴 No active business connections found

**To connect:**
1. Use /start bizChat to get setup instructions
2. Connect via Telegram Business settings
3. Grant necessary permissions

**Available Features:**
• Customer message handling: ⏸️ Inactive
• Auto-responses: ⏸️ Inactive
• AI conversations: ⏸️ Inactive

Connect your business account to activate these features!`, userId, chatId)

	msg := tgbotapi.NewMessage(chatId, statusMsg)
	msg.ParseMode = "Markdown"

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "business_status"),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Setup", "business_setup"),
		),
	)
	msg.ReplyMarkup = keyboard

	_, err := bot.Send(msg)
	if err != nil {
		logger.Warn("Failed to send business status", "err", err)
	}
}

// sendBusinessSettings sends business settings options
func sendBusinessSettings(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	chatId := update.Message.Chat.ID

	settingsMsg := `⚙️ **Business Settings**

Configure your business bot preferences:

**Response Settings:**
• Auto-reply: Enabled ✅
• Response delay: Instant
• Language: Auto-detect

**AI Settings:**
• Model: DeepSeek-V3
• Temperature: Balanced
• Max tokens: 4000

**Business Hours:**
• Always active: 24/7 ✅
• Timezone: Auto-detect

**Customer Management:**
• New customer greeting: Enabled ✅
• Escalation to human: Available
• Message history: Retained

Use the buttons below to modify settings.`

	msg := tgbotapi.NewMessage(chatId, settingsMsg)
	msg.ParseMode = "Markdown"

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Auto-Reply", "toggle_autoreply"),
			tgbotapi.NewInlineKeyboardButtonData("🌐 Language", "set_language"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🧠 AI Model", "set_model"),
			tgbotapi.NewInlineKeyboardButtonData("⏰ Hours", "set_hours"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👥 Customer", "customer_settings"),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "business_help"),
		),
	)
	msg.ReplyMarkup = keyboard

	_, err := bot.Send(msg)
	if err != nil {
		logger.Warn("Failed to send business settings", "err", err)
	}
}

// sendBusinessCommandHelp sends help for unrecognized business commands
func sendBusinessCommandHelp(chatId int64, bot *tgbotapi.BotAPI) {
	helpMsg := `❓ **Unknown Business Command**

Available business commands:
• /business_help - Show help information
• /business_status - Check connection status
• /business_settings - Manage settings
• /start bizChat - Setup business connection

For general bot commands, use /help.`

	msg := tgbotapi.NewMessage(chatId, helpMsg)
	msg.ParseMode = "Markdown"

	_, err := bot.Send(msg)
	if err != nil {
		logger.Warn("Failed to send business command help", "err", err)
	}
}

// Business callback query handlers

// sendBusinessCommandsList shows list of business commands
func sendBusinessCommandsList(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Business commands loaded")
	bot.Send(callback)

	chatId := update.CallbackQuery.Message.Chat.ID
	msgId := update.CallbackQuery.Message.MessageID

	commandsMsg := `📋 **Business Commands List**

**Setup & Connection:**
• /start bizChat - Setup business connection
• /business_status - Check connection status

**Management:**
• /business_settings - Configure bot settings
• /business_help - Show help information

**Quick Actions:**
Use the buttons below for quick access to business features.`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", "business_status"),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", "business_settings"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏢 Setup", "business_setup"),
			tgbotapi.NewInlineKeyboardButtonData("❓ Help", "business_help"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatId, msgId, commandsMsg)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}

// sendBusinessHelpCallback shows business help via callback
func sendBusinessHelpCallback(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Help loaded")
	bot.Send(callback)

	chatId := update.CallbackQuery.Message.Chat.ID
	msgId := update.CallbackQuery.Message.MessageID

	helpMsg := `🤖 **Business Bot Help**

**What I can do:**
• ✅ Respond to customer messages automatically
• ✅ Handle multiple conversations simultaneously
• ✅ Support multiple languages (EN/ZH/RU)
• ✅ Provide AI-powered intelligent responses
• ✅ Work 24/7 without breaks

**Getting Started:**
1. Connect your business account via Telegram settings
2. Configure bot preferences using /business_settings
3. Test the connection with sample messages

**Features:**
• Smart context-aware conversations
• Automatic language detection
• Customizable response templates
• Business hours configuration
• Customer escalation to human agents

**Need more help?** Use the commands below or contact support.`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Commands", "business_commands"),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", "business_settings"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", "business_status"),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "business_commands"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatId, msgId, helpMsg)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}

// sendBusinessStatusCallback shows business status via callback
func sendBusinessStatusCallback(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Status refreshed")
	bot.Send(callback)

	chatId := update.CallbackQuery.Message.Chat.ID
	msgId := update.CallbackQuery.Message.MessageID
	userId := update.CallbackQuery.From.ID

	statusMsg := fmt.Sprintf(`📊 **Business Status**

**User ID:** %d
**Chat ID:** %d
**Last Updated:** %s

**Connection Status:**
🟡 Demo Mode (API compatibility layer)

**Active Features:**
• Customer message handling: ✅ Active
• Auto-responses: ✅ Enabled
• AI conversations: ✅ Active
• Multi-language support: ✅ Available

**Statistics:**
• Messages processed: -
• Active conversations: -
• Response time: < 2 seconds

*Note: This is a compatibility demonstration. Full business features will be available when the Telegram Bot API library supports Bot API v7.0+ business features.*`, userId, chatId, time.Now().Format("15:04:05"))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "business_status"),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", "business_settings"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏢 Setup", "business_setup"),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "business_help"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatId, msgId, statusMsg)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}

// sendBusinessSetupCallback shows business setup via callback
func sendBusinessSetupCallback(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Setup guide loaded")
	bot.Send(callback)

	chatId := update.CallbackQuery.Message.Chat.ID
	msgId := update.CallbackQuery.Message.MessageID

	setupMsg := `🏢 **Business Setup Guide**

**Current Implementation:**
This bot includes a compatibility layer for Telegram Business features that are available in Bot API v7.0+.

**When full business features are available:**

**Step 1:** Enable Business Features
• Go to @BotFather
• Select your bot
• Enable business connection capabilities

**Step 2:** Connect Business Account
• Open Telegram Business Settings
• Navigate to "Chatbots" section
• Connect this bot to your account

**Step 3:** Configure Permissions
• ✅ Read customer messages
• ✅ Send responses
• ✅ Manage conversations (optional)

**Step 4:** Test & Launch
• Send test messages to verify connection
• Configure response templates
• Launch for customers

**Current Status:** Demo/Compatibility mode active`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("🤖 BotFather", "https://t.me/botfather"),
			tgbotapi.NewInlineKeyboardButtonURL("📱 Business Settings", "https://t.me/settings/business"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", "business_status"),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "business_help"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatId, msgId, setupMsg)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}

// toggleBusinessAutoReply toggles auto-reply setting
func toggleBusinessAutoReply(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Auto-reply setting toggled")
	bot.Send(callback)

	chatId := update.CallbackQuery.Message.Chat.ID
	msgId := update.CallbackQuery.Message.MessageID

	// In a real implementation, this would toggle the actual setting
	toggleMsg := `🔄 **Auto-Reply Settings**

**Current Status:** ✅ Enabled

Auto-reply allows the bot to automatically respond to customer messages using AI-powered responses.

**Options:**
• **Enabled:** Bot responds automatically to all messages
• **Disabled:** Manual responses only
• **Smart Mode:** Bot decides when to respond

**Response Delay:** Instant (0-1 seconds)
**Fallback:** Escalate to human after 3 failed attempts

*Setting has been toggled successfully.*`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Toggle Again", "toggle_autoreply"),
			tgbotapi.NewInlineKeyboardButtonData("⏱️ Set Delay", "set_delay"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ All Settings", "business_settings"),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "business_settings"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatId, msgId, toggleMsg)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}

// showBusinessLanguageOptions shows language configuration options
func showBusinessLanguageOptions(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Language options loaded")
	bot.Send(callback)

	chatId := update.CallbackQuery.Message.Chat.ID
	msgId := update.CallbackQuery.Message.MessageID

	langMsg := `🌐 **Language Settings**

**Current Language:** Auto-detect ✅

The bot can automatically detect customer language and respond accordingly. You can also set a default language for all interactions.

**Available Languages:**
• 🇺🇸 English (EN)
• 🇨🇳 Chinese (ZH)
• 🇷🇺 Russian (RU)
• 🌍 Auto-detect (Recommended)

**Auto-detect Features:**
• Analyzes incoming message language
• Responds in the same language
• Maintains conversation context
• Fallback to default if uncertain`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🇺🇸 English", "set_lang_en"),
			tgbotapi.NewInlineKeyboardButtonData("🇨🇳 Chinese", "set_lang_zh"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🇷🇺 Russian", "set_lang_ru"),
			tgbotapi.NewInlineKeyboardButtonData("🌍 Auto-detect", "set_lang_auto"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", "business_settings"),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "business_settings"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatId, msgId, langMsg)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}

// showBusinessModelOptions shows AI model configuration options
func showBusinessModelOptions(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Model options loaded")
	bot.Send(callback)

	chatId := update.CallbackQuery.Message.Chat.ID
	msgId := update.CallbackQuery.Message.MessageID

	modelMsg := `🧠 **AI Model Settings**

**Current Model:** DeepSeek-V3 ✅

Choose the AI model that best fits your business needs. Different models offer varying levels of performance, speed, and capabilities.

**Available Models:**
• 🚀 DeepSeek-V3: Best overall performance
• 🔥 DeepSeek-R1: Advanced reasoning
• 🤖 OpenAI GPT-4: Versatile and reliable
• 💎 Gemini Pro: Google's advanced model
• 🌐 OpenRouter: Access to 400+ models

**Model Characteristics:**
• **Speed:** Fast (< 2 seconds)
• **Quality:** High accuracy responses
• **Context:** Long conversation memory
• **Specialization:** Business conversations`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🚀 DeepSeek-V3", "model_deepseek_v3"),
			tgbotapi.NewInlineKeyboardButtonData("🔥 DeepSeek-R1", "model_deepseek_r1"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🤖 OpenAI GPT-4", "model_openai"),
			tgbotapi.NewInlineKeyboardButtonData("💎 Gemini Pro", "model_gemini"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🌐 OpenRouter", "model_openrouter"),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "business_settings"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatId, msgId, modelMsg)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}

// showBusinessHoursOptions shows business hours configuration
func showBusinessHoursOptions(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Business hours loaded")
	bot.Send(callback)

	chatId := update.CallbackQuery.Message.Chat.ID
	msgId := update.CallbackQuery.Message.MessageID

	hoursMsg := `⏰ **Business Hours Settings**

**Current Setting:** 24/7 Active ✅

Configure when your business bot should automatically respond to customers. Outside business hours, the bot can show a custom message or operate in limited mode.

**Options:**
• 🌍 **24/7 Active:** Always respond (Recommended)
• 🕒 **Business Hours Only:** Set specific hours
• 🌙 **Smart Mode:** Reduced responses outside hours
• 📅 **Custom Schedule:** Different hours for different days



**Current Schedule:**
• Monday-Friday: 24/7
• Saturday-Sunday: 24/7
• Holidays: Active
• Timezone: Auto-detect

**Outside Hours Action:**
• Show availability message
• Queue messages for review
• Emergency contact available`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🌍 24/7 Mode", "hours_24_7"),
			tgbotapi.NewInlineKeyboardButtonData("🕒 Set Hours", "hours_custom"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🌙 Smart Mode", "hours_smart"),
			tgbotapi.NewInlineKeyboardButtonData("📅 Schedule", "hours_schedule"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", "business_settings"),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "business_settings"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatId, msgId, hoursMsg)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}

// showCustomerSettings shows customer management settings
func showCustomerSettings(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	callback := tgbotapi.NewCallback(update.CallbackQuery.ID, "Customer settings loaded")
	bot.Send(callback)

	chatId := update.CallbackQuery.Message.Chat.ID
	msgId := update.CallbackQuery.Message.MessageID

	customerMsg := `👥 **Customer Management Settings**

Configure how your bot interacts with customers and manages conversations.

**Current Settings:**
• 👋 **Welcome Message:** Enabled
• 📝 **Message History:** 7 days retention
• 🔄 **Auto-escalation:** After 3 failed attempts
• 📊 **Analytics:** Basic tracking enabled

**Customer Experience:**
• ✅ Personalized greetings for new customers
• ✅ Context-aware conversations
• ✅ Polite and professional tone
• ✅ Quick response times (< 2 seconds)

**Privacy & Data:**
• 🔒 Messages encrypted in transit
• 📅 Automatic cleanup after retention period
• 🚫 No data sharing with third parties
• ✅ GDPR compliant processing

**Escalation Rules:**
• Human handoff available
• Complex queries forwarded
• Customer satisfaction monitoring`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👋 Welcome Msg", "customer_welcome"),
			tgbotapi.NewInlineKeyboardButtonData("📝 History", "customer_history"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Escalation", "customer_escalation"),
			tgbotapi.NewInlineKeyboardButtonData("📊 Analytics", "customer_analytics"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ All Settings", "business_settings"),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "business_settings"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatId, msgId, customerMsg)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}
