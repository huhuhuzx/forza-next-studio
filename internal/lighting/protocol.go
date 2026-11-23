package lighting

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	CmdReadRAM          byte = 0xA3
	CmdWriteRAM         byte = 0xA4
	LEDRAMID            byte = 0x04
	RAMReadChunkSize         = 16
	MaxLightingRAMSize       = 4096
	LightModeSolid      byte = 1
	FlydigiVID               = uint16(0x37D7)
	FlydigiK6PID             = uint16(0x2502)
	FlydigiConfigPage        = uint16(0xFFA0)
)

// Sum16 computes the unsigned 16-bit modular byte sum used by Flydigi LGHT headers/payloads.
func Sum16(b []byte) uint16 {
	var s uint16
	for _, v := range b {
		s += uint16(v)
	}
	return s
}

// CRC16CCITT computes CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF) matching k6-telemetry-console.
func CRC16CCITT(b []byte) uint16 {
	var crc uint16 = 0xFFFF
	for _, v := range b {
		crc ^= uint16(v) << 8
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// CRC32IEEE computes standard IEEE CRC32 used in 0xA4 BEGIN/COMMIT packets.
func CRC32IEEE(b []byte) uint32 {
	return crc32.ChecksumIEEE(b)
}

// BuildReport33 constructs a 33-byte HID output report (ReportID=0x00 + 32B frame) for 0xA3/0xA4 commands.
func BuildReport33(reportID byte, cmd byte, payload []byte) ([33]byte, error) {
	var out [33]byte
	if len(payload)+5 > 32 {
		return out, fmt.Errorf("lighting frame too long: %d bytes", len(payload)+5)
	}
	out[0] = reportID
	out[1] = 0x5A
	out[2] = 0xA5
	out[3] = cmd
	out[4] = byte(len(payload) + 2)
	copy(out[5:], payload)
	var sum byte
	for i := 3; i < 5+len(payload); i++ {
		sum += out[i]
	}
	out[5+len(payload)] = sum
	return out, nil
}

// ParseResponse extracts the command response slice (starting at declaredLength byte, excluding checksum),
// matching runtime-core.mjs including the zero-padded fixed-report compatibility fallback.
func ParseResponse(bytes []byte, cmd byte) []byte {
	offset := -1
	for i := 0; i < len(bytes)-1; i++ {
		if bytes[i] == 0x5A && bytes[i+1] == 0xA5 {
			offset = i
			break
		}
	}
	if offset < 0 || offset+5 > len(bytes) || bytes[offset+2] != cmd {
		return nil
	}
	hasChecksumAt := func(candidateEnd int) bool {
		var sum byte
		for i := offset + 2; i < candidateEnd-1; i++ {
			sum += bytes[i]
		}
		return sum == bytes[candidateEnd-1]
	}
	declaredLen := int(bytes[offset+3])
	end := offset + 3 + declaredLen
	if declaredLen >= 2 && end <= len(bytes) && hasChecksumAt(end) {
		res := make([]byte, end-1-(offset+3))
		copy(res, bytes[offset+3:end-1])
		return res
	}
	for candidateEnd := len(bytes); candidateEnd >= offset+5; candidateEnd-- {
		zeroPadded := true
		for i := candidateEnd; i < len(bytes); i++ {
			if bytes[i] != 0 {
				zeroPadded = false
				break
			}
		}
		if zeroPadded && hasChecksumAt(candidateEnd) {
			res := make([]byte, candidateEnd-1-(offset+3))
			copy(res, bytes[offset+3:candidateEnd-1])
			return res
		}
	}
	return nil
}

// ReadRAMChunk holds a parsed 0xA3 READ_RAM chunk reply.
type ReadRAMChunk struct {
	ActiveSlot  byte
	TotalLength int
	ChunkIndex  byte
	Data        []byte
}

// ParseReadRAMChunk parses and validates a 0xA3 reply payload.
func ParseReadRAMChunk(payload []byte, expectedChunkIndex byte) (ReadRAMChunk, error) {
	if len(payload) < 8 {
		return ReadRAMChunk{}, fmt.Errorf("灯光读取回包过短：%dB", len(payload))
	}
	status := payload[1]
	ramID := payload[2]
	activeSlot := payload[3]
	totalLength := int(binary.LittleEndian.Uint16(payload[4:6]))
	chunkIndex := payload[6]
	chunkSize := int(payload[7])
	if status != 0 {
		return ReadRAMChunk{}, fmt.Errorf("灯光读取失败，chunk %d，状态 %d", expectedChunkIndex, status)
	}
	if ramID != LEDRAMID {
		return ReadRAMChunk{}, fmt.Errorf("灯光读取 RAM 不一致：0x%02x", ramID)
	}
	if chunkIndex != expectedChunkIndex {
		return ReadRAMChunk{}, fmt.Errorf("灯光读取分块不一致：%d/%d", chunkIndex, expectedChunkIndex)
	}
	if totalLength <= 0 || totalLength > MaxLightingRAMSize {
		return ReadRAMChunk{}, fmt.Errorf("灯光 RAM 长度异常：%dB", totalLength)
	}
	avail := len(payload) - 8
	if chunkSize <= 0 || chunkSize > avail {
		return ReadRAMChunk{}, fmt.Errorf("灯光读取分块 %d 长度异常：声明 %dB，实际 %dB", chunkIndex, chunkSize, avail)
	}
	data := make([]byte, chunkSize)
	copy(data, payload[8:8+chunkSize])
	return ReadRAMChunk{
		ActiveSlot:  activeSlot,
		TotalLength: totalLength,
		ChunkIndex:  chunkIndex,
		Data:        data,
	}, nil
}

// EncodeLightingPayload wraps a raw LGHT payload with the 12-byte LGHT v2 header + 2-byte trailer checksum.
func EncodeLightingPayload(payload []byte) []byte {
	hdr := make([]byte, 12)
	copy(hdr[0:4], []byte("LGHT"))
	binary.LittleEndian.PutUint16(hdr[4:6], 2)
	binary.LittleEndian.PutUint32(hdr[6:10], uint32(len(payload)))
	binary.LittleEndian.PutUint16(hdr[10:12], Sum16(hdr[0:10]))

	out := make([]byte, 12+len(payload)+2)
	copy(out[0:12], hdr)
	copy(out[12:12+len(payload)], payload)
	binary.LittleEndian.PutUint16(out[12+len(payload):], Sum16(payload))
	return out
}

// EncodeSolidLightingBin encodes a solid RGB + brightness + vibrationLink LGHT blob.
func EncodeSolidLightingBin(r, g, b, brightness byte, vibrationLink bool) []byte {
	if brightness > 100 {
		brightness = 100
	}
	vib := byte(0)
	if vibrationLink {
		vib = 1
	}
	return EncodeLightingPayload([]byte{LightModeSolid, r, g, b, brightness, vib})
}

// WrapLightingBin prepends the 4-byte A4 transport wrapper (u16 totalLen, u16 crc16Ccitt(bin)).
func WrapLightingBin(bin []byte) []byte {
	w := make([]byte, 4+len(bin))
	binary.LittleEndian.PutUint16(w[0:2], uint16(len(w)))
	binary.LittleEndian.PutUint16(w[2:4], CRC16CCITT(bin))
	copy(w[4:], bin)
	return w
}

// ValidateLightingBin checks LGHT magic, header checksum, length, and payload checksum.
func ValidateLightingBin(bin []byte) error {
	if len(bin) < 14 {
		return errors.New("灯光 RAM 内容过短")
	}
	if string(bin[0:4]) != "LGHT" {
		return errors.New("灯光 RAM 缺少 LGHT 标识")
	}
	if binary.LittleEndian.Uint16(bin[10:12]) != Sum16(bin[0:10]) {
		return errors.New("灯光 RAM 头校验失败")
	}
	pLen := int(binary.LittleEndian.Uint32(bin[6:10]))
	if 12+pLen+2 != len(bin) {
		return fmt.Errorf("灯光 RAM 内容长度不一致：%d/%d", 12+pLen+2, len(bin))
	}
	if binary.LittleEndian.Uint16(bin[12+pLen:14+pLen]) != Sum16(bin[12:12+pLen]) {
		return errors.New("灯光 RAM 内容校验失败")
	}
	return nil
}

// ValidateLightingRAM checks the 4-byte A4 wrapper (length + CRC16) and inner LGHT blob.
func ValidateLightingRAM(wrapped []byte) error {
	if len(wrapped) < 18 {
		return errors.New("灯光 RAM 数据过短")
	}
	declLen := int(binary.LittleEndian.Uint16(wrapped[0:2]))
	if declLen != len(wrapped) {
		return fmt.Errorf("灯光 RAM 包装长度不一致：%d/%d", declLen, len(wrapped))
	}
	bin := wrapped[4:]
	if binary.LittleEndian.Uint16(wrapped[2:4]) != CRC16CCITT(bin) {
		return errors.New("灯光 RAM CRC16 校验失败")
	}
	return ValidateLightingBin(bin)
}

// ValidateRestorableSnapshot accepts wrapped-lght, direct lght, or firmware opaque presets,
// rejecting empty/all-zero/all-0xFF blobs (exact match to runtime-core.mjs).
func ValidateRestorableSnapshot(snapshot []byte) (string, error) {
	if len(snapshot) == 0 || len(snapshot) > MaxLightingRAMSize {
		return "", fmt.Errorf("灯光快照长度异常：%dB", len(snapshot))
	}
	if len(snapshot) >= 4 && string(snapshot[0:4]) == "LGHT" {
		if err := ValidateLightingBin(snapshot); err != nil {
			return "", err
		}
		return "lght", nil
	}
	if len(snapshot) >= 18 && string(snapshot[4:8]) == "LGHT" {
		if err := ValidateLightingRAM(snapshot); err != nil {
			return "", err
		}
		return "wrapped-lght", nil
	}
	allZero, allFF := true, true
	for _, b := range snapshot {
		if b != 0x00 {
			allZero = false
		}
		if b != 0xFF {
			allFF = false
		}
	}
	if allZero || allFF {
		return "", errors.New("灯光快照是空白数据，不能用于恢复")
	}
	return "opaque", nil
}
