package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLength  = 16
	argonKeyLength   = 32
)

// HashPassword returns an argon2id hash of password with a random 16-byte
// salt, in the "$argon2id$v=19$m=...,t=...,p=...$salt$hash" form that
// VerifyPassword reads. Passwords shorter than 10 bytes are rejected.
func HashPassword(password string) (string, error) {
	if len(password) < 10 {
		return "", errors.New("webui password must contain at least 10 characters")
	}
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

// VerifyPassword reports whether password matches an encoded argon2id hash.
// A malformed hash never matches, and neither does one whose parameters,
// salt or key length fall outside fixed bounds, so a crafted hash cannot
// demand excessive memory or time. The final comparison is constant-time.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 {
		return false
	}
	memory, err1 := parseArgonParameter(parameters[0], "m")
	iterations, err2 := parseArgonParameter(parameters[1], "t")
	parallelism, err3 := parseArgonParameter(parameters[2], "p")
	if err1 != nil || err2 != nil || err3 != nil || memory < 8*parallelism || memory > 256*1024 || iterations < 1 || iterations > 10 || parallelism < 1 || parallelism > 16 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) < 16 || len(expected) > 64 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(memory), uint8(parallelism), uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

// parseArgonParameter parses one "name=value" element of the argon2 parameter
// list.
func parseArgonParameter(value, name string) (uint64, error) {
	prefix := name + "="
	if !strings.HasPrefix(value, prefix) {
		return 0, errors.New("invalid parameter")
	}
	return strconv.ParseUint(strings.TrimPrefix(value, prefix), 10, 32)
}
