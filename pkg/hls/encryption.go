package hls

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
)

type encryption struct {
	uri        string
	iv         [aes.BlockSize]byte
	explicitIV bool
}

func parseEncryption(attrs map[string]string, base *url.URL) (encryption, error) {
	if attrs["METHOD"] == "NONE" {
		return encryption{}, nil
	}
	if attrs["METHOD"] != "AES-128" || attrs["KEYFORMAT"] != "" && attrs["KEYFORMAT"] != "identity" {
		return encryption{}, errors.New("hls: only identity AES-128 encryption is supported; use ffmpeg")
	}
	r, err := resolveResource(base, attrs["URI"], "", resource{})
	if err != nil {
		return encryption{}, err
	}
	e := encryption{uri: r.uri}
	if value, ok := attrs["IV"]; ok {
		if !strings.HasPrefix(value, "0x") && !strings.HasPrefix(value, "0X") {
			return e, errors.New("hls: invalid encryption IV")
		}
		value = value[2:]
		if value == "" || len(value) > 32 {
			return e, errors.New("hls: invalid encryption IV")
		}
		if len(value)%2 != 0 {
			value = "0" + value
		}
		b, err := hex.DecodeString(value)
		if err != nil {
			return e, errors.New("hls: invalid encryption IV")
		}
		copy(e.iv[16-len(b):], b)
		e.explicitIV = true
	}
	return e, nil
}

func (r *reader) decrypt(data []byte, e encryption, sequence uint64) ([]byte, error) {
	if e.uri == "" {
		return data, nil
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("hls: invalid AES-128 ciphertext length")
	}
	// Refetch to permit key rotation at a stable URI; HTTP transports can reuse connections.
	key, _, err := r.fetch(resource{uri: e.uri}, aes.BlockSize)
	if err != nil {
		return nil, err
	}
	if len(key) != aes.BlockSize {
		return nil, errors.New("hls: AES-128 key must be 16 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := e.iv
	if !e.explicitIV {
		binary.BigEndian.PutUint64(iv[8:], sequence)
	}
	cipher.NewCBCDecrypter(block, iv[:]).CryptBlocks(data, data)
	n := int(data[len(data)-1])
	if n == 0 || n > aes.BlockSize || n > len(data) {
		return nil, errors.New("hls: invalid AES-128 padding")
	}
	for _, v := range data[len(data)-n:] {
		if int(v) != n {
			return nil, errors.New("hls: invalid AES-128 padding")
		}
	}
	return data[:len(data)-n], nil
}
