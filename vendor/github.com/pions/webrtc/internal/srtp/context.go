package srtp

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1" // #nosec
	"encoding/binary"

	"github.com/pkg/errors"
)

const (
	labelSRTPEncryption        = 0x00
	labelSRTPAuthenticationTag = 0x01
	labelSRTPSalt              = 0x02

	labelSRTCPEncryption        = 0x03
	labelSRTCPAuthenticationTag = 0x04
	labelSRTCPSalt              = 0x05

	keyLen  = 16
	saltLen = 14

	maxROCDisorder    = 100
	maxSequenceNumber = 65535

	authTagSize    = 10
	srtcpIndexSize = 4
)

// Encode/Decode state for a single SSRC
type ssrcState struct {
	ssrc                 uint32
	rolloverCounter      uint32
	rolloverHasProcessed bool
	lastSequenceNumber   uint16
}

// Context represents a SRTP cryptographic context
// Context can only be used for one-way operations
// it must either used ONLY for encryption or ONLY for decryption
type Context struct {
	masterKey  []byte
	masterSalt []byte

	ssrcStates         map[uint32]*ssrcState
	srtpSessionKey     []byte
	srtpSessionSalt    []byte
	srtpSessionAuthTag []byte
	srtpBlock          cipher.Block

	srtcpSessionKey     []byte
	srtcpSessionSalt    []byte
	srtcpSessionAuthTag []byte
	srtcpIndex          uint32
	srtcpBlock          cipher.Block
}

// CreateContext creates a new SRTP Context
func CreateContext(masterKey, masterSalt []byte, profile string) (c *Context, err error) {
	if masterKeyLen := len(masterKey); masterKeyLen != keyLen {
		return c, errors.Errorf("SRTP Master Key must be len %d, got %d", masterKey, keyLen)
	} else if masterSaltLen := len(masterSalt); masterSaltLen != saltLen {
		return c, errors.Errorf("SRTP Salt must be len %d, got %d", saltLen, masterSaltLen)
	}

	c = &Context{
		masterKey:  masterKey,
		masterSalt: masterSalt,
		ssrcStates: map[uint32]*ssrcState{},
	}

	if c.srtpSessionKey, err = c.generateSessionKey(labelSRTPEncryption); err != nil {
		return nil, err
	} else if c.srtpSessionSalt, err = c.generateSessionSalt(labelSRTPSalt); err != nil {
		return nil, err
	} else if c.srtpSessionAuthTag, err = c.generateSessionAuthTag(labelSRTPAuthenticationTag); err != nil {
		return nil, err
	} else if c.srtpBlock, err = aes.NewCipher(c.srtpSessionKey); err != nil {
		return nil, err
	}

	if c.srtcpSessionKey, err = c.generateSessionKey(labelSRTCPEncryption); err != nil {
		return nil, err
	} else if c.srtcpSessionSalt, err = c.generateSessionSalt(labelSRTCPSalt); err != nil {
		return nil, err
	} else if c.srtcpSessionAuthTag, err = c.generateSessionAuthTag(labelSRTCPAuthenticationTag); err != nil {
		return nil, err
	} else if c.srtcpBlock, err = aes.NewCipher(c.srtcpSessionKey); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Context) generateSessionKey(label byte) ([]byte, error) {
	// https://tools.ietf.org/html/rfc3711#appendix-B.3
	// The input block for AES-CM is generated by exclusive-oring the master salt with the
	// concatenation of the encryption key label 0x00 with (index DIV kdr),
	// - index is 'rollover count' and DIV is 'divided by'
	sessionKey := make([]byte, len(c.masterSalt))
	copy(sessionKey, c.masterSalt)

	labelAndIndexOverKdr := []byte{label, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for i, j := len(labelAndIndexOverKdr)-1, len(sessionKey)-1; i >= 0; i, j = i-1, j-1 {
		sessionKey[j] = sessionKey[j] ^ labelAndIndexOverKdr[i]
	}

	// then padding on the right with two null octets (which implements the multiply-by-2^16 operation, see Section 4.3.3).
	sessionKey = append(sessionKey, []byte{0x00, 0x00}...)

	//The resulting value is then AES-CM- encrypted using the master key to get the cipher key.
	block, err := aes.NewCipher(c.masterKey)
	if err != nil {
		return nil, err
	}

	block.Encrypt(sessionKey, sessionKey)
	return sessionKey, nil
}

func (c *Context) generateSessionSalt(label byte) ([]byte, error) {
	// https://tools.ietf.org/html/rfc3711#appendix-B.3
	// The input block for AES-CM is generated by exclusive-oring the master salt with
	// the concatenation of the encryption salt label
	sessionSalt := make([]byte, len(c.masterSalt))
	copy(sessionSalt, c.masterSalt)

	labelAndIndexOverKdr := []byte{label, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for i, j := len(labelAndIndexOverKdr)-1, len(sessionSalt)-1; i >= 0; i, j = i-1, j-1 {
		sessionSalt[j] = sessionSalt[j] ^ labelAndIndexOverKdr[i]
	}

	// That value is padded and encrypted as above.
	sessionSalt = append(sessionSalt, []byte{0x00, 0x00}...)
	block, err := aes.NewCipher(c.masterKey)
	if err != nil {
		return nil, err
	}

	block.Encrypt(sessionSalt, sessionSalt)
	return sessionSalt[0:saltLen], nil
}
func (c *Context) generateSessionAuthTag(label byte) ([]byte, error) {
	// https://tools.ietf.org/html/rfc3711#appendix-B.3
	// We now show how the auth key is generated.  The input block for AES-
	// CM is generated as above, but using the authentication key label.
	sessionAuthTag := make([]byte, len(c.masterSalt))
	copy(sessionAuthTag, c.masterSalt)

	labelAndIndexOverKdr := []byte{label, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for i, j := len(labelAndIndexOverKdr)-1, len(sessionAuthTag)-1; i >= 0; i, j = i-1, j-1 {
		sessionAuthTag[j] = sessionAuthTag[j] ^ labelAndIndexOverKdr[i]
	}

	// That value is padded and encrypted as above.
	// - We need to do multiple runs at key size (20) is larger then source
	firstRun := append(sessionAuthTag, []byte{0x00, 0x00}...)
	secondRun := append(sessionAuthTag, []byte{0x00, 0x01}...)
	block, err := aes.NewCipher(c.masterKey)
	if err != nil {
		return nil, err
	}

	block.Encrypt(firstRun, firstRun)
	block.Encrypt(secondRun, secondRun)
	return append(firstRun, secondRun[:4]...), nil
}

// Generate IV https://tools.ietf.org/html/rfc3711#section-4.1.1
// where the 128-bit integer value IV SHALL be defined by the SSRC, the
// SRTP packet index i, and the SRTP session salting key k_s, as below.
// - ROC = a 32-bit unsigned rollover counter (ROC), which records how many
// -       times the 16-bit RTP sequence number has been reset to zero after
// -       passing through 65,535
// i = 2^16 * ROC + SEQ
// IV = (salt*2 ^ 16) | (ssrc*2 ^ 64) | (i*2 ^ 16)
func (c *Context) generateCounter(sequenceNumber uint16, rolloverCounter uint32, ssrc uint32, sessionSalt []byte) []byte {
	counter := make([]byte, 16)

	binary.BigEndian.PutUint32(counter[4:], ssrc)
	binary.BigEndian.PutUint32(counter[8:], rolloverCounter)
	binary.BigEndian.PutUint32(counter[12:], uint32(sequenceNumber)<<16)

	for i := range sessionSalt {
		counter[i] = counter[i] ^ sessionSalt[i]
	}

	return counter
}

func (c *Context) generateAuthTag(buf, sessionAuthTag []byte) ([]byte, error) {
	// https://tools.ietf.org/html/rfc3711#section-4.2
	// In the case of SRTP, M SHALL consist of the Authenticated
	// Portion of the packet (as specified in Figure 1) concatenated with
	// the ROC, M = Authenticated Portion || ROC;
	//
	// The pre-defined authentication transform for SRTP is HMAC-SHA1
	// [RFC2104].  With HMAC-SHA1, the SRTP_PREFIX_LENGTH (Figure 3) SHALL
	// be 0.  For SRTP (respectively SRTCP), the HMAC SHALL be applied to
	// the session authentication key and M as specified above, i.e.,
	// HMAC(k_a, M).  The HMAC output SHALL then be truncated to the n_tag
	// left-most bits.
	// - Authenticated portion of the packet is everything BEFORE MKI
	// - k_a is the session message authentication key
	// - n_tag is the bit-length of the output authentication tag
	// - ROC is already added by caller (to allow RTP + RTCP support)
	mac := hmac.New(sha1.New, sessionAuthTag)

	if _, err := mac.Write(buf); err != nil {
		return nil, err
	}

	return mac.Sum(nil)[0:10], nil
}

func (c *Context) verifyAuthTag(buf, actualAuthTag []byte) (bool, error) {
	expectedAuthTag, err := c.generateAuthTag(buf, c.srtpSessionAuthTag)
	if err != nil {
		return false, err
	}
	return bytes.Equal(actualAuthTag, expectedAuthTag), nil
}
