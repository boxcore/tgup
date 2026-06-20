package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func main() {
	var flagArgs []string
	var positionalArgs []string
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if !strings.Contains(arg, "=") {
				fName := strings.TrimLeft(arg, "-")
				if fName == "title" || fName == "t" || fName == "test" || fName == "type" || fName == "n" || fName == "s" || fName == "sort" || fName == "spil" || fName == "r" || fName == "rate-limit" {
					if i+1 < len(os.Args) {
						flagArgs = append(flagArgs, os.Args[i+1])
						i++
					}
				}
			}
		} else {
			positionalArgs = append(positionalArgs, arg)
		}
	}
	os.Args = append(append([]string{os.Args[0]}, flagArgs...), positionalArgs...)

	var titleFlag, testFlag, typeFilterFlag, sortFlag string
	var cacheForceFlag, spilFlag, checkVedioFlag, versionFlag bool
	var batchSizeFlag, sleepDurationFlag, rateLimitFlag int

	flag.StringVar(&titleFlag, "title", "", "Global title")
	flag.StringVar(&testFlag, "t", "", "Test mode")
	flag.StringVar(&testFlag, "test", "", "Test mode long")
	flag.BoolVar(&cacheForceFlag, "cache-force", false, "Force metadata")
	flag.BoolVar(&cacheForceFlag, "cf", false, "Force metadata short")
	flag.IntVar(&batchSizeFlag, "n", 10, "Batch size")
	flag.StringVar(&typeFilterFlag, "type", "all", "Type filter")
	flag.IntVar(&sleepDurationFlag, "s", 4, "Sleep duration")
	flag.StringVar(&sortFlag, "sort", "name", "Sort strategy")
	flag.BoolVar(&spilFlag, "spil", false, "Force split")
	flag.BoolVar(&checkVedioFlag, "check-vedio", false, "Check video files info")
	flag.BoolVar(&checkVedioFlag, "check-video", false, "Check video files info (alias)")
	flag.IntVar(&rateLimitFlag, "rate-limit", -1, "Max messages per minute")
	flag.IntVar(&rateLimitFlag, "r", -1, "Max messages per minute (short)")
	flag.BoolVar(&versionFlag, "version", false, "Show version info")
	flag.BoolVar(&versionFlag, "v", false, "Show version info (alias)")
	flag.Parse()

	if versionFlag {
		fmt.Println("v0.1")
		os.Exit(0)
	}

	tailArgs := flag.Args()
	if len(tailArgs) == 0 {
		fmt.Println("Error: Please specify a path.")
		os.Exit(1)
	}
	targetPath := tailArgs[0]

	finalTestMode := testFlag
	if finalTestMode == "" {
		finalTestMode = flag.Lookup("test").Value.String()
	}
	if batchSizeFlag > 10 || batchSizeFlag <= 0 {
		batchSizeFlag = 10
	}
	if sleepDurationFlag < 0 {
		sleepDurationFlag = 0
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		os.Exit(1)
	}
	baseDir := filepath.Join(homeDir, ".tgup")
	cacheDir, logDir := filepath.Join(baseDir, "cache"), filepath.Join(baseDir, "logs")
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(logDir, 0755)

	config, err := loadConfig(filepath.Join(baseDir, "config.conf"))
	if err != nil {
		os.Exit(1)
	}
	if spilFlag {
		config.AllowSpilFile = true
	}
	if rateLimitFlag != -1 {
		config.RateLimit = rateLimitFlag
	}

	rawFiles, err := collectFiles(targetPath, sortFlag)
	if err != nil {
		os.Exit(1)
	}

	var files []string
	typeFilterFlag = strings.ToLower(strings.TrimSpace(typeFilterFlag))
	for _, file := range rawFiles {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto, isVideo := contains(config.PhotoExts, ext), contains(config.VideoExts, ext)
		if typeFilterFlag != "all" && typeFilterFlag != "pic" && typeFilterFlag != "video" && typeFilterFlag != "vedio" {
			tExt := typeFilterFlag
			if !strings.HasPrefix(tExt, ".") {
				tExt = "." + tExt
			}
			if ext != tExt {
				continue
			}
		} else {
			if typeFilterFlag == "pic" && !isPhoto {
				continue
			}
			if (typeFilterFlag == "video" || typeFilterFlag == "vedio") && !isVideo {
				continue
			}
		}
		if !isPhoto && !isVideo {
			continue
		}
		files = append(files, file)
	}

	if len(files) == 0 {
		fmt.Println("No valid files found.")
		return
	}

	if checkVedioFlag {
		var videoFiles []string
		for _, file := range files {
			ext := strings.ToLower(filepath.Ext(file))
			if contains(config.VideoExts, ext) {
				videoFiles = append(videoFiles, file)
			}
		}
		if len(videoFiles) == 0 {
			fmt.Println("No video files found to check.")
			return
		}
		checkVideoFiles(videoFiles, cacheDir)
		return
	}

	if finalTestMode == "" {
		files = filterUnsupportedVideos(files, config, cacheDir)
		if len(files) == 0 {
			fmt.Println("No files left to upload after filtering.")
			return
		}
	}

	if finalTestMode == "list" {
		runs := buildRuns(files, config)
		fmt.Printf("\n📋 预检就绪：过滤与排序完成的文件列表 (共 %d 个有效文件):\n", len(files))
		fmt.Println(strings.Repeat("-", 60))
		globalIdx, batchNo := 0, 0
		for _, run := range runs {
			if run.largeVideoPath != "" {
				fi, statErr := os.Stat(run.largeVideoPath)
				sizeStr := "Unknown"
				if statErr == nil {
					sizeStr = formatSize(fi.Size())
				}
				globalIdx++
				fmt.Printf("[%03d] %s (%s) — 🎬 超限大视频，运行时自动切片后单独分批投递（具体切片数取决于时长，此处不预估）\n", globalIdx, filepath.Base(run.largeVideoPath), sizeStr)
				fmt.Println(strings.Repeat("-", 60))
				continue
			}
			for _, chunk := range chunkBySizeAndCount(run.normalFiles, batchSizeFlag, math.MaxInt64) {
				batchNo++
				for _, file := range chunk {
					globalIdx++
					fi, err := os.Stat(file)
					sizeStr, cTimeStr, mTimeStr := "Unknown", "--", "--"
					if err == nil {
						sizeStr = formatSize(fi.Size())
						mTime := fi.ModTime()
						cTime := mTime
						if sys := fi.Sys(); sys != nil {
							v := reflect.ValueOf(sys).Elem()
							statField := v.FieldByName("Ctim")
							if !statField.IsValid() {
								statField = v.FieldByName("Ctimespec")
							}
							if statField.IsValid() {
								secField := statField.FieldByName("Sec")
								nsecField := statField.FieldByName("Nsec")
								if secField.IsValid() && nsecField.IsValid() {
									cTime = time.Unix(secField.Int(), nsecField.Int())
								}
							}
						}
						cTimeStr, mTimeStr = cTime.Format("2006-01-02 15:04"), mTime.Format("2006-01-02 15:04")
					}
					fmt.Printf("[%03d] %s (%s | C:%s M:%s)\n", globalIdx, filepath.Base(file), sizeStr, cTimeStr, mTimeStr)
				}
				fmt.Printf("📦 👆 以上为 [第 %d 批] 普通相册预检包 (包含 %d 个文件) 👆\n", batchNo, len(chunk))
				fmt.Println(strings.Repeat("-", 60))
			}
		}
		fmt.Printf("✅ 预计产生 %d 个普通相册批次（大视频切片产生的额外批次数运行时才能确定，不含在内）\n", batchNo)
		return
	}

	globalCaption := strings.TrimSpace(titleFlag)
	if globalCaption == "" && finalTestMode != "curl" {
		fmt.Print("Do you want to add a caption for this upload? (Press Enter to skip): ")
		input, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		globalCaption = strings.TrimSpace(input)
	}

	var bot *tgbotapi.BotAPI
	if finalTestMode != "curl" {
		bot, err = tgbotapi.NewBotAPIWithAPIEndpoint(config.BotAPI, config.TgAPIURL+"/bot%s/%s")
		if err != nil {
			os.Exit(1)
		}
	}

	runs := buildRuns(files, config)
	for _, run := range runs {
		if run.largeVideoPath != "" {
			handleSplitVideoUpload(bot, config, run.largeVideoPath, cacheDir, cacheForceFlag, globalCaption, logDir, finalTestMode, batchSizeFlag, sleepDurationFlag)
			if sleepDurationFlag > 0 {
				time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
			}
			continue
		}
		chunks := chunkBySizeAndCount(run.normalFiles, batchSizeFlag, math.MaxInt64)
		for i, chunk := range chunks {
			fmt.Printf("\n--- Preparing batch: 第 %d/%d 批，共 %d 个文件，发起投递 ---\n", i+1, len(chunks), len(chunk))
			preProcessAndUpload(bot, config, chunk, cacheDir, cacheForceFlag, globalCaption, logDir, finalTestMode)
			if sleepDurationFlag > 0 {
				time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
			}
		}
	}
	if finalTestMode != "curl" {
		fmt.Println("\nAll uploads completed successfully!")
	}
}

