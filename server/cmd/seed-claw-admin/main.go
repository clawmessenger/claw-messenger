// seed-claw-admin：创建默认管理员 admin/admin123（不存在时）。
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	u := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:4])
		u = u[:0]
		u = prf.Sum(u)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(t)
			t = t[:0]
			t = prf.Sum(t)
			for j := range t {
				u[j] ^= t[j]
			}
		}
		dk = append(dk, u...)
	}
	return dk[:keyLen]
}

func clawHashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	dk := pbkdf2SHA256([]byte(password), salt, 100_000, 32)
	return fmt.Sprintf("pbkdf2_sha256$100000$%s$%s", hex.EncodeToString(salt), hex.EncodeToString(dk))
}

func main() {
	dbURL := os.Getenv("MULTICA_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica"
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		fmt.Println("connect error:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	var count int
	if err := conn.QueryRow(ctx, "SELECT COUNT(*) FROM claw_admin_users").Scan(&count); err != nil {
		fmt.Println("query error:", err)
		os.Exit(1)
	}
	if count > 0 {
		fmt.Println("admin users already exist:", count)
		return
	}
	hash := clawHashPassword("admin123")
	tag, err := conn.Exec(ctx,
		`INSERT INTO claw_admin_users (admin_id, username, nickname, password_hash, role_id)
		 VALUES ('admin-1', 'admin', 'Administrator', $1, 'super_admin')`, hash)
	if err != nil {
		fmt.Println("insert error:", err)
		os.Exit(1)
	}
	fmt.Println("seeded admin/admin123 rows:", tag.RowsAffected())
}
