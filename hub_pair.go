package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

func runPairCommand(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not configured. Run: ccc setup <bot_token>")
	}
	id, err := loadOrCreateHubIdentity()
	if err != nil {
		return err
	}
	db, err := openStore(dbPath(cfg))
	if err != nil {
		return err
	}
	code := newHubPairCode()
	row := HubPairCode{Code: code, ExpiresAt: time.Now().Add(pairCodeTTLHub)}
	if err := db.Create(&row).Error; err != nil {
		return err
	}
	hub := hubURLFromConfig(cfg)
	uri := pairingURI(hub, id.ID(), id.PublicHex(), code)
	fmt.Printf("Pair this machine with the CCC app. Code expires in 10 minutes.\n\n")
	fmt.Printf("  Machine:  %s\n", instanceDisplayName(cfg))
	fmt.Printf("  Hub:      %s\n", hub)
	fmt.Printf("  Code:     %s\n\n", strings.ToUpper(code))
	fmt.Printf("URI (scan or paste in the app):\n  %s\n", uri)
	if len(args) > 0 && args[0] == "--uri" {
		return nil
	}
	return nil
}

func pairingURI(hub, instanceID, pubHex, code string) string {
	q := url.Values{}
	q.Set("h", hub)
	q.Set("i", instanceID)
	q.Set("k", pubHex)
	q.Set("n", code)
	return "ccc://pair/v1?" + q.Encode()
}

func newHubPairCode() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := time.Now().UnixNano()
		b[0], b[1], b[2] = byte(n), byte(n>>8), byte(n>>16)
	}
	return hex.EncodeToString(b[:])
}

func runUnpairCommand(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openStore(dbPath(cfg))
	if err != nil {
		return err
	}
	if len(args) == 0 {
		var devices []HubDevice
		db.Order("paired_at").Find(&devices)
		if len(devices) == 0 {
			fmt.Println("No paired devices.")
			return nil
		}
		fmt.Println("Paired devices:")
		for _, d := range devices {
			seen := "never"
			if d.LastSeen != nil {
				seen = d.LastSeen.Local().Format(time.RFC3339)
			}
			fmt.Printf("  %s  %s  last %s\n", d.PubKey[:12], d.Name, seen)
		}
		fmt.Println("\nRevoke with: ccc unpair <id-prefix-or-name>")
		return nil
	}
	q := strings.TrimSpace(args[0])
	res := db.Where("pub_key LIKE ? OR name = ?", q+"%", q).Delete(&HubDevice{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("no device matched %q", q)
	}
	fmt.Printf("Revoked %d device(s).\n", res.RowsAffected)
	return nil
}

