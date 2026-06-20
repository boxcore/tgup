package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ChatID        int64    `json:"chat_id"`
	BotAPI        string   `json:"bot_api"`
	TgAPIURL      string   `json:"tg_api_url"`
	PhotoExts     []string `json:"photo_exts"`
	VideoExts     []string `json:"video_exts"`
	AllowSpilFile bool     `json:"allow_spil_file"`
	AllowMaxSize  int64    `json:"allow_max_size"`
	SpilMaxSize   float64  `json:"spil_max_size"`
	RateLimit     int      `json:"rate_limit"`
}

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	cfg := &Config{
		PhotoExts:     []string{".jpg", ".jpeg", ".png"},
		VideoExts:     []string{".mp4", ".mkv", ".avi", ".mov", ".m4v"},
		AllowSpilFile: false,
		AllowMaxSize:  10 * 1024 * 1024 * 1024,
		SpilMaxSize:   1.5,
		RateLimit:     20,
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := strings.ToUpper(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])

		switch key {
		case "CHAT_ID":
			id, _ := strconv.ParseInt(val, 10, 64)
			cfg.ChatID = id
		case "BOT_API":
			cfg.BotAPI = val
		case "TG_API_URL":
			cfg.TgAPIURL = val
		case "RATE_LIMIT":
			limit, _ := strconv.Atoi(val)
			cfg.RateLimit = limit
		case "ALLOW_SPIL_FILE":
			cfg.AllowSpilFile = strings.ToLower(val) == "true"
		case "ALLOW_MAX_SIZE":
			vL := strings.ToLower(val)
			if strings.HasSuffix(vL, "g") {
				num, _ := strconv.ParseInt(strings.TrimSuffix(vL, "g"), 10, 64)
				cfg.AllowMaxSize = num * 1024 * 1024 * 1024
			} else if strings.HasSuffix(vL, "m") {
				num, _ := strconv.ParseInt(strings.TrimSuffix(vL, "m"), 10, 64)
				cfg.AllowMaxSize = num * 1024 * 1024
			} else {
				num, _ := strconv.ParseInt(val, 10, 64)
				cfg.AllowMaxSize = num
			}
		case "SPIL_MAX_SIZE":
			f, _ := strconv.ParseFloat(val, 64)
			cfg.SpilMaxSize = f
		case "PHOTO_EXTS", "VIDEO_EXTS":
			exts := strings.Split(strings.ToLower(val), ",")
			var clean []string
			for _, e := range exts {
				e = strings.TrimSpace(e)
				if e != "" {
					if !strings.HasPrefix(e, ".") {
						e = "." + e
					}
					clean = append(clean, e)
				}
			}
			if len(clean) > 0 {
				if key == "PHOTO_EXTS" {
					cfg.PhotoExts = clean
				} else {
					cfg.VideoExts = clean
				}
			}
		}
	}
	return cfg, scanner.Err()
}
