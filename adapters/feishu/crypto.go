package feishu

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// signature 计算飞书回调签名：hex(sha256(timestamp + nonce + encryptKey + body))。
func signature(timestamp, nonce, encryptKey string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(timestamp))
	h.Write([]byte(nonce))
	h.Write([]byte(encryptKey))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// verifySignature 校验回调签名，encryptKey 为空时视为无需校验。
func verifySignature(encryptKey, timestamp, nonce, got string, body []byte) bool {
	if encryptKey == "" {
		return true
	}
	if got == "" {
		return false
	}
	want := signature(timestamp, nonce, encryptKey, body)
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// secureEqual 以常数时间比较两个字符串，避免时序侧信道。
func secureEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// decryptEvent 解密飞书加密事件。
//
// key 为 sha256(encryptKey) 得到的 32 字节；密文是 base64 编码的
// AES-256-CBC 数据，其前 16 字节为 IV，其余为 PKCS7 填充的明文。
// 解密失败一律返回错误，不 panic。
func decryptEvent(encryptKey, encrypt string) ([]byte, error) {
	if encryptKey == "" {
		return nil, errors.New("feishu: 回调已加密但未配置 EncryptKey")
	}
	raw, err := base64.StdEncoding.DecodeString(encrypt)
	if err != nil {
		return nil, fmt.Errorf("feishu: 事件密文不是合法 base64: %w", err)
	}
	if len(raw) <= aes.BlockSize || (len(raw)-aes.BlockSize)%aes.BlockSize != 0 {
		return nil, errors.New("feishu: 事件密文长度非法")
	}
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("feishu: 初始化 AES 失败: %w", err)
	}
	iv, data := raw[:aes.BlockSize], raw[aes.BlockSize:]
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, data)
	plain, err = unpadPKCS7(plain)
	if err != nil {
		return nil, err
	}
	return plain, nil
}

// unpadPKCS7 去掉 PKCS7 填充，填充非法时返回错误。
func unpadPKCS7(b []byte) ([]byte, error) {
	n := len(b)
	if n == 0 {
		return nil, errors.New("feishu: 解密后明文为空")
	}
	pad := int(b[n-1])
	if pad == 0 || pad > aes.BlockSize || pad > n {
		return nil, errors.New("feishu: PKCS7 填充长度非法")
	}
	for _, c := range b[n-pad:] {
		if int(c) != pad {
			return nil, errors.New("feishu: PKCS7 填充内容非法")
		}
	}
	return b[:n-pad], nil
}
