package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	color_extractor "github.com/marekm4/color-extractor"
	"github.com/zmb3/spotify/v2"
	spotifyauth "github.com/zmb3/spotify/v2/auth"
	"golang.org/x/oauth2"
)

var (
	currentState  *spotify.CurrentlyPlaying
	stateMutex    sync.Mutex
	imageCacheDir string
	reportURL     string
)

func main() {
	godotenv.Load(".env")
	httpPort := os.Getenv("HTTP_PORT")
	imageCacheDir = os.Getenv("IMAGE_CACHE_DIR")
	if imageCacheDir == "" {
		imageCacheDir = "image_cache"
	}
	reportURL = os.Getenv("REPORT_URL")

	logger := log.New(os.Stdout, "spotify-reporter: ", log.LstdFlags)

	auth := spotifyauth.New(
		spotifyauth.WithClientID(os.Getenv("SPOTIFY_CLIENT_ID")),
		spotifyauth.WithClientSecret(os.Getenv("SPOTIFY_CLIENT_SECRET")),
		spotifyauth.WithRedirectURL(os.Getenv("SPOTIFY_REDIRECT_URL")),
		spotifyauth.WithScopes(spotifyauth.ScopeUserReadPlaybackState),
	)

	// Get the authentication URL
	url := auth.AuthURL("state")
	fmt.Println("Please visit this URL to authorize the application:", url)

	// Set up a web server to handle the OAuth callback
	ch := make(chan *oauth2.Token)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Println("Received request:", r.URL.String())

		token, err := auth.Token(r.Context(), "state", r)
		if err != nil {
			http.Error(w, "Couldn't get token", http.StatusForbidden)
			logger.Fatal(err)
		}

		if st := r.FormValue("state"); st != "state" {
			http.NotFound(w, r)
			logger.Fatalf("State mismatch: %s != state", st)
		}

		// Print the token details
		// logger.Printf("Access token: %s\n", token.AccessToken)
		// logger.Printf("Refresh token: %s\n", token.RefreshToken)
		logger.Printf("Token type: %s\n", token.TokenType)
		logger.Printf("Expires in: %d seconds\n", token.Expiry.Unix()-time.Now().Unix())

		// Display a success message
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "Success! You can now close this window.")

		// Send the token through a channel
		ch <- token
	})
	go http.ListenAndServe(":"+httpPort, nil)

	// Wait for the user to authorize the application and get the token
	token := <-ch

	// Create a new Spotify client using the token
	client := spotify.New(auth.Client(context.Background(), token))

	// Start a goroutine to send the current status every 1 second
	go func() {
		for {
			stateMutex.Lock()
			if currentState != nil && currentState.Playing {
				sendCurrentStatus(currentState)
			}
			stateMutex.Unlock()
			time.Sleep(1 * time.Second)
		}
	}()

	for {
		// Get the current playback state
		state, err := client.PlayerCurrentlyPlaying(context.Background())
		if err != nil {
			fmt.Println("Error getting playback state:", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Update the shared state
		stateMutex.Lock()
		state.Timestamp = time.Now().UnixNano() / int64(time.Millisecond)
		currentState = state
		stateMutex.Unlock()

		// Sleep for 5 seconds before the next query
		time.Sleep(5 * time.Second)
	}
}

func sendCurrentStatus(state *spotify.CurrentlyPlaying) {
	if state == nil {
		return
	}

	// Calculate the elapsed time since the last update
	elapsedTime := time.Since(time.Unix(int64(state.Timestamp/1000), 0))

	// Adjust the progress based on the elapsed time
	adjustedProgress := state.Progress + int(elapsedTime/time.Millisecond)
	if adjustedProgress >= state.Item.Duration {
		adjustedProgress = state.Item.Duration
	}

	// Get the album art URL and extract colors
	albumArtColors := []color.Color{}
	albumArtUrl := getAlbumArtURL(state.Item.Album)
	if albumArtUrl != "" {
		fmt.Println("Album art URL:", albumArtUrl)
		albumArtFile, err := downloadAlbumArt(albumArtUrl)
		if err != nil {
			fmt.Println("Error downloading album art:", err)
		}
		if albumArtFile != "" {
			albumArtColors = extractColors(albumArtFile)
			fmt.Println("Colors extracted:", albumArtColors)
		}
	}

	// Default color if no colors are extracted
	albumArtColor := []uint32{248, 236, 235}
	// Get the first color if any colors are extracted
	if len(albumArtColors) > 0 {
		r, g, b, _ := albumArtColors[0].RGBA()
		albumArtColor = []uint32{r >> 8, g >> 8, b >> 8}
	}
	// Convert each uint32 to string
	albumArtColorStrs := make([]string, len(albumArtColor))
	for i, num := range albumArtColor {
		albumArtColorStrs[i] = strconv.Itoa(int(num))
	}
	// Join the string slice with commas
	albumArtColorStr := strings.Join(albumArtColorStrs, ",")
	fmt.Println("Album art color:", albumArtColorStr)

	// Text color based on album art color
	textColorRGBA := provideTextColor(color.RGBA{uint8(albumArtColor[0]), uint8(albumArtColor[1]), uint8(albumArtColor[2]), 255})
	fmt.Println("Text color RGBA:", textColorRGBA)
	textColor := []uint32{uint32(textColorRGBA.R), uint32(textColorRGBA.G), uint32(textColorRGBA.B)}
	// Convert each uint32 to string
	textColorStrs := make([]string, len(textColor))
	for i, num := range textColor {
		textColorStrs[i] = strconv.Itoa(int(num))
	}
	// Join the string slice with commas
	textColorStr := strings.Join(textColorStrs, ",")
	fmt.Println("Text color:", textColorStr)

	// Create a report object with adjusted progress
	report := map[string]interface{}{
		"timestamp":           time.Now().Unix(),
		"playback_state":      state.Playing,
		"track":               state.Item.Name,
		"album":               state.Item.Album.Name,
		"artist":              state.Item.Artists[0].Name,
		"endpoint":            state.PlaybackContext.Endpoint,
		"progress_pct":        float64(adjustedProgress) / float64(state.Item.Duration) * 100,
		"progress_pct_str":    fmt.Sprintf("%.2f%%", float64(adjustedProgress)/float64(state.Item.Duration)*100),
		"progress_ms":         adjustedProgress,
		"duration_ms":         state.Item.Duration,
		"remaining_ms":        state.Item.Duration - adjustedProgress,
		"progress_str":        fmt.Sprintf("%d:%02d", adjustedProgress/60000, (adjustedProgress/1000)%60),
		"duration_str":        fmt.Sprintf("%d:%02d", state.Item.Duration/60000, (state.Item.Duration/1000)%60),
		"remaining_str":       fmt.Sprintf("%d:%02d", (state.Item.Duration-adjustedProgress)/60000, ((state.Item.Duration-adjustedProgress)/1000)%60),
		"album_art_url":       getAlbumArtURL(state.Item.Album),
		"album_art_color_rgb": albumArtColorStr,
		"album_art_colors":    albumArtColors,
		"text_color_rgb":      textColorStr,
		"text_color":          textColor,
	}

	// Convert the report to JSON
	jsonData, err := json.Marshal(report)
	if err != nil {
		fmt.Println("Error marshaling JSON:", err)
		return
	}

	// Send the report via POST request
	resp, err := http.Post(reportURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Println("Error sending POST request:", err)
	} else {
		resp.Body.Close()
		fmt.Println("Current status reported")
	}
}

func getAlbumArtURL(album spotify.SimpleAlbum) string {
	if len(album.Images) > 0 {
		return album.Images[0].URL
	}
	return ""
}

func downloadAlbumArt(url string) (filename string, err error) {
	// Check if the URL is empty
	if url == "" {
		fmt.Println("Album art URL is empty")
		return
	}

	// Check if the URL is valid
	var headResp *http.Response
	headResp, err = http.Head(url)
	if err != nil {
		fmt.Println("Invalid album art URL:", err)
		return
	}

	// Check if the URL is an image
	if headResp.Header.Get("Content-Type") != "image/jpeg" {
		fmt.Println("Invalid album art URL: not an image")
		return
	}

	// Get the image file name
	filename = filepath.Join(imageCacheDir, filepath.Base(url)) + ".jpeg"

	// Check if the URL is already cached
	if _, err = os.Stat(filename); err == nil {
		fmt.Println("Album art already cached")
		return
	}

	// Download the album art image
	var resp *http.Response
	resp, err = http.Get(url)
	if err != nil {
		fmt.Println("Error downloading album art:", err)
		return
	}
	defer resp.Body.Close()

	// Create the image cache directory if it doesn't exist
	if _, err = os.Stat(imageCacheDir); os.IsNotExist(err) {
		os.Mkdir(imageCacheDir, 0755)
	}

	// Save the image to a file
	var file *os.File
	file, err = os.Create(filename)
	if err != nil {
		fmt.Println("Error creating file:", err)
		return
	}

	// Copy the image data to the file
	if _, err = io.Copy(file, resp.Body); err != nil {
		fmt.Println("Error saving image:", err)
		return
	}

	fmt.Println("Album art saved to:", filename)
	return
}

func extractColors(imagePath string) []color.Color {
	imageFile, _ := os.Open(imagePath)
	defer imageFile.Close()

	image, format, err := image.Decode(imageFile)
	if err != nil {
		fmt.Println("Error decoding image:", err)
		return nil
	}
	fmt.Println("Image format:", format)
	colors := color_extractor.ExtractColors(image)

	return colors
}

func relativeLuminance(c color.RGBA) float64 {
	// Convert RGB values to sRGB
	r := sRGBToLinear(float64(c.R) / 255.0)
	g := sRGBToLinear(float64(c.G) / 255.0)
	b := sRGBToLinear(float64(c.B) / 255.0)

	// Calculate relative luminance
	return 0.2126*r + 0.7152*g + 0.0722*b
}

func sRGBToLinear(c float64) float64 {
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func calculateContrastRatio(l1, l2 float64) float64 {
	// Ensure l1 is the lighter color
	if l1 < l2 {
		l1, l2 = l2, l1
	}

	// Calculate contrast ratio
	return (l1 + 0.05) / (l2 + 0.05)
}

func calculateColorContrast(c1, c2 color.RGBA) float64 {
	// Calculate relative luminance for each color
	l1 := relativeLuminance(c1)
	l2 := relativeLuminance(c2)

	// Calculate contrast ratio
	return calculateContrastRatio(l1, l2)
}

func provideTextColor(bgColor color.RGBA) color.RGBA {
	// Calculate the contrast ratio with black and white
	blackContrast := calculateColorContrast(bgColor, color.RGBA{0, 0, 0, 255})
	whiteContrast := calculateColorContrast(bgColor, color.RGBA{255, 255, 255, 255})

	// Return black or white based on the higher contrast ratio
	if blackContrast > whiteContrast {
		return color.RGBA{0, 0, 0, 255}
	}
	return color.RGBA{255, 255, 255, 255}
}