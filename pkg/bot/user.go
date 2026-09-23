package bot

// User 是消息发送者或 @ 的目标用户。
type User struct {
	// ID 是平台内的用户唯一标识。
	ID string
	// Name 是用户显示名。
	Name string
	// IsBot 表示该用户是否为机器人。
	IsBot bool
}

// Channel 是消息所在的会话（群、频道、私聊会话）。
type Channel struct {
	// ID 是平台内的会话唯一标识。
	ID string
	// Name 是会话显示名。
	Name string
	// Kind 是会话类型。
	Kind MessageKind
}
