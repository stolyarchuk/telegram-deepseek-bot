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
