# GitHub Copilot Instructions for telegram-deepseek-bot

## Project Overview

This is a **Telegram bot** built with **Go 1.24+** that integrates multiple AI providers including **DeepSeek**, **OpenAI**, **Gemini**, **Volcengine**, and **OpenRouter**. The bot supports multimodal interactions (text, voice, images, video), streaming responses, MCP (Model Context Protocol) for function calling, RAG (Retrieval-Augmented Generation), and comprehensive user management.

## Architecture & Code Patterns

### Project Structure

```
telegram-deepseek-bot/
├── main.go                 # Application entry point
├── conf/                   # Configuration management
├── llm/                    # LLM provider implementations
├── robot/                  # Telegram bot logic
├── db/                     # Database operations
├── utils/                  # Utility functions
├── logger/                 # Logging system
├── metrics/                # Prometheus metrics & pprof
├── rag/                    # RAG implementation
├── i18n/                   # Internationalization
├── param/                  # Parameter definitions
├── static/                 # Static assets & documentation
└── test/                   # Test utilities
```

### Core Components

#### 1. LLM Providers (`llm/` directory)

- **DeepSeek**: Primary AI provider with V3 and R1 models
- **OpenAI**: GPT models integration
- **Gemini**: Google's AI models (Gemini 2.5 Pro/Flash, 2.0 Flash, etc.)
- **Volcengine**: ByteDance's AI services for images/video
- **OpenRouter**: Access to 400+ LLMs
- **Ollama**: Local model support

**Pattern**: Each provider implements the `LLMClient` interface with methods:

```go
type LLMClient interface {
    CallLLMAPI(ctx context.Context, prompt string, l *LLM) error
    GetMessages(userId int64, prompt string)
    Send(ctx context.Context, l *LLM) error
    GetUserMessage(msg string)
    GetAssistantMessage(msg string)
    AppendMessages(client LLMClient)
}
```

#### 2. Bot Architecture (`robot/` directory)

- Main bot logic with update handling
- Command processing (/clear, /retry, /mode, /balance, /state, /photo, /video, /chat, /help)
- Admin commands (/addtoken)
- Streaming response implementation
- User rate limiting and chat management

#### 3. Configuration (`conf/` directory)

- Environment variable management
- Tool configuration for MCP
- Multi-language support (EN/ZH/RU)
- Database configuration (SQLite3/MySQL)

#### 4. Database (`db/` directory)

- User management with token tracking
- Message recording
- RAG file management
- SQLite3 and MySQL support

### Key Programming Patterns

#### 1. Error Handling

Always use structured error handling with context:

```go
if err != nil {
    logger.Error("Operation failed", "error", err, "context", contextInfo)
    return fmt.Errorf("failed to perform operation: %w", err)
}
```

#### 2. Logging

Use the custom logger with structured fields:

```go
logger.Info("Processing request", "userId", userId, "operation", "chat")
logger.Error("API call failed", "provider", "deepseek", "error", err)
```

#### 3. Configuration Access

Use global configuration variables:

```go
*conf.TelegramBotToken
*conf.DeepseekToken
*conf.MaxUserChat
```

#### 4. Internationalization

Use i18n for user-facing messages:

```go
i18n.SendMsg(chatId, "message_key", bot, params, msgId)
```

#### 5. Metrics

Record metrics for monitoring:

```go
metrics.TotalRecords.Inc()
metrics.ConversationDuration.Observe(duration.Seconds())
```

### Naming Conventions

#### Variables & Functions

- Use camelCase for Go conventions
- Prefix boolean variables with `is`, `has`, `need`
- Use descriptive names: `getUserMessage`, `processStreamResponse`

#### Constants

- Use PascalCase for exported constants
- Group related constants in `param/` package
- Provider names: `DeepSeek`, `OpenAi`, `Gemini`, `Vol`, `OpenRouter`

#### File Organization

- One main struct per file in `llm/` directory
- Test files use `_test.go` suffix
- Group related functionality in packages

### API Integration Patterns

#### 1. HTTP Clients

Always use context with timeouts:

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
```

#### 2. Streaming Responses

Implement streaming for better UX:

```go
// Process streaming response chunks
for chunk := range responseStream {
    // Handle partial content
    // Update message incrementally
}
```

#### 3. Error Recovery

Implement retry logic for API failures:

```go
for attempts := 0; attempts < maxRetries; attempts++ {
    if err := callAPI(); err == nil {
        break
    }
    time.Sleep(backoffDuration)
}
```

### Testing Guidelines

#### 1. Unit Tests

- Use testify/assert for assertions
- Mock external dependencies
- Test error conditions
- Example: `utils/utils_test.go`, `logger/logger_test.go`

#### 2. Integration Tests

- Test with mock HTTP clients
- Validate end-to-end workflows
- Use test utilities from `test/` package

### Security Considerations

#### 1. Token Management

- Never log sensitive tokens
- Use environment variables for secrets
- Implement user token quotas

#### 2. Input Validation

- Validate all user inputs
- Sanitize file uploads
- Rate limit API calls

#### 3. Access Control

- Check user permissions for admin commands
- Validate chat types (private/group)
- Implement user whitelisting

### Performance Guidelines

#### 1. Concurrent Processing

- Use goroutines for independent operations
- Implement proper synchronization with sync.Map
- Handle graceful shutdowns

#### 2. Memory Management

- Use context for cancellation
- Clean up resources in defer statements
- Implement connection pooling for databases

#### 3. Caching

- Cache frequently accessed data
- Use appropriate TTL values
- Implement cache invalidation strategies

### Common Tasks & Patterns

#### 1. Adding New LLM Provider

1. Create new file in `llm/` directory
2. Implement `LLMClient` interface
3. Add provider constants to `param/`
4. Update configuration handling
5. Add provider-specific error handling

#### 2. Adding New Bot Command

1. Add command handler in `robot/`
2. Register command in `utils/utils.go`
3. Add i18n translations
4. Update documentation in `static/doc/`

#### 3. Adding New Configuration

1. Define in appropriate `conf/` file
2. Add environment variable parsing
3. Update configuration documentation
4. Add validation if needed

### Dependencies & Libraries

#### Core Dependencies

- `github.com/go-telegram-bot-api/telegram-bot-api/v5` - Telegram Bot API
- `github.com/cohesion-org/deepseek-go` - DeepSeek API client
- `github.com/sashabaranov/go-openai` - OpenAI API client
- `google.golang.org/genai` - Google Gemini API
- `github.com/yincongcyincong/mcp-client-go` - MCP protocol client

#### Database & Storage

- `github.com/mattn/go-sqlite3` - SQLite3 driver
- `github.com/go-sql-driver/mysql` - MySQL driver

#### Utilities

- `github.com/rs/zerolog` - Structured logging
- `github.com/prometheus/client_golang` - Metrics
- `github.com/stretchr/testify` - Testing framework

### Environment Variables

Key environment variables to be aware of:

- `TELEGRAM_BOT_TOKEN` - Telegram bot token (required)
- `DEEPSEEK_TOKEN` - DeepSeek API key (required)
- `TYPE` - Model type (deepseek/openai/gemini/openrouter/vol)
- `DB_TYPE` - Database type (sqlite3/mysql)
- `USE_TOOLS` - Enable MCP function calling
- `LANG` - Language (en/zh/ru)

### Build Tags

The project uses build tags:

- `//go:build !libtokenizers` in main.go for conditional compilation

When working on this codebase, prioritize:

1. **Type Safety**: Use strong typing for all API interactions
2. **Error Handling**: Implement comprehensive error handling with context
3. **Performance**: Use streaming and concurrent processing where appropriate
4. **Security**: Validate inputs and manage tokens securely
5. **Maintainability**: Follow established patterns and document complex logic
6. **Testing**: Write tests for new features and bug fixes
7. **Internationalization**: Support multiple languages for user-facing content

Always refer to existing implementations in the codebase for patterns and consistency.
