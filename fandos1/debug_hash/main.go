package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

func main() {
	// Test different combinations of parameters to find the correct data_check_string format
	initData := "query_id=AAHdF6IQAAAAAN0XohDhrOrc&user=%7B%22id%22%3A279058397%2C%22first_name%22%3A%22dev%22%2C%22last_name%22%3A%22%22%2C%22username%22%3A%22devuser%22%2C%22language_code%22%3A%22en%22%7D&auth_date=1696588919&hash=0d623aa3f2670e88e3f6d23e44d3a0d536e0c92f5f63a6e8821b67e63a5f0b25"
	botToken := "5768337691:AAH5YkoiWVoyDnuT8P5jznJuUeK5MEZK8_4"
	expectedHash := "0d623aa3f2670e88e3f6d23e44d3a0d536e0c92f5f63a6e8821b67e63a5f0b25"

	// Try different parameter combinations
	testCombinations := [][]string{
		{"auth_date", "query_id", "user"},  // all params (alphabetical)
		{"auth_date", "user"},               // without query_id
		{"auth_date", "query_id"},           // without user
		{"query_id", "user"},                // without auth_date
		{"user"},                             // only user
		{"auth_date"},                        // only auth_date
		{"query_id"},                         // only query_id
	}

	// Compute secret_key
	hmac256 := hmac.New(sha256.New, []byte("WebAppData"))
	hmac256.Write([]byte("5768337691:AAH5YkoiWVoyDnuT8P5jznJuUeK5MEZK8_4"))
	secretKey := hmac256.Sum(nil)
	fmt.Printf("secret_key: %s\n", hex.EncodeToString(secretKey))

	// Parse initData into raw key=value pairs
	type pair struct{ key, val string }
	var allPairs []pair
	for _, p := range strings.Split(initData, "&") {
		if p == "" { continue }
		eq := strings.Index(p, "=")
		if eq < 0 { continue }
		k := p[:eq]
		v := p[eq+1:]
		if k != "hash" {
			fmt.Printf("  %s=%s\n", k, v)
		}
	}

	// Try all combinations
	for _, combo := range testCombinations {
		var parts []string
		for _, p := range strings.Split(initData, "&") {
			if p == "" { continue }
			eq := strings.Index(p, "=")
			if eq < 0 { continue }
			k := p[:eq]
			v := p[eq+1:]
			if k == "hash" { continue }
			
			// Check if this key is in our combo
			found := false
			for _, c := range combo {
				if c == k {
					found = true
					break
				}
			}
			if found {
				parts = append(parts, k+"="+v)
			}
		}
		sort.Strings(parts)
		dataCheckString := strings.Join(parts, "\n")
		
		// Compute hash
		hmac3 := hmac.New(sha256.New, secretKey)
		hmac3.Write([]byte(dataCheckString))
		hash := hex.EncodeToString(hmac3.Sum(nil))
		
		match := strings.EqualFold(hash, expectedHash)
		fmt.Printf("Combo %v: hash=%s match=%v\n", combo, hash, match)
		fmt.Printf("  data_check_string:\n%s\n\n", strings.Join(parts, "\n"))
	}
}