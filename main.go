package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
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

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/lucasb-eyer/go-colorful"
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
	upgrader      = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	wsClients = make(map[*websocket.Conn]bool)
)

func main() {
	godotenv.Load(".env")
	httpPort := os.Getenv("HTTP_PORT")
	wsURL := os.Getenv("WS_URL")
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
	tokenReceiverHandler := func(w http.ResponseWriter, r *http.Request) {
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
		logger.Printf("Token type: %s\n", token.TokenType)
		logger.Printf("Expires in: %d seconds\n", token.Expiry.Unix()-time.Now().Unix())

		// Display a success message
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "Success! You can now close this window. <a href='./'>./</a>")

		// Send the token through a channel
		ch <- token
	}
	http.HandleFunc("/callback", tokenReceiverHandler)
	http.HandleFunc("/_spotifymonitor/callback", tokenReceiverHandler)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		state := getCurrentStatus(currentState)
		state["httpPort"] = httpPort
		state["wsURL"] = wsURL
		tmpl := template.Must(template.New("").Parse(playerTemplate))
		err := tmpl.Execute(w, state)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/_spotifymonitor/ws", handleWebSocket)

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

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	wsClients[conn] = true

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			delete(wsClients, conn)
			break
		}
	}
}

func getCurrentStatus(state *spotify.CurrentlyPlaying) map[string]interface{} {
	if state == nil {
		return nil
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

	// Progress color based on album art color
	progressColorRGBA_Triadic1, progressColorRGBA_Triadic2 := provideTriadicColors(color.RGBA{uint8(albumArtColor[0]), uint8(albumArtColor[1]), uint8(albumArtColor[2]), 255})
	progressColorRGBA_Analogous1, progressColorRGBA_Analogous2 := provideAnalogousColors(color.RGBA{uint8(albumArtColor[0]), uint8(albumArtColor[1]), uint8(albumArtColor[2]), 255})
	progressColorRGBA_Complementary := provideComplementaryColor(color.RGBA{uint8(albumArtColor[0]), uint8(albumArtColor[1]), uint8(albumArtColor[2]), 255})
	progressColorRGBA := chooseBestContrastingColor(
		color.RGBA{uint8(albumArtColor[0]), uint8(albumArtColor[1]), uint8(albumArtColor[2]), 255},
		[]color.RGBA{
			progressColorRGBA_Triadic1,
			progressColorRGBA_Triadic2,
			progressColorRGBA_Analogous1,
			progressColorRGBA_Analogous2,
			progressColorRGBA_Complementary,
		})
	fmt.Println("Progress color RGBA:", progressColorRGBA)
	progressColor := []uint32{uint32(progressColorRGBA.R), uint32(progressColorRGBA.G), uint32(progressColorRGBA.B)}
	// Convert each uint32 to string
	progressColorStrs := make([]string, len(progressColor))
	for i, num := range progressColor {
		progressColorStrs[i] = strconv.Itoa(int(num))
	}
	// Join the string slice with commas
	progressColorStr := strings.Join(progressColorStrs, ",")
	fmt.Println("Progress color:", progressColorStr)

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
		"progress_color_rgb":  progressColorStr,
		"progress_color":      progressColor,
	}

	return report
}

func sendCurrentStatus(state *spotify.CurrentlyPlaying) {
	if state == nil {
		return
	}

	// Get the status details
	report := getCurrentStatus(state)

	// Convert the report to JSON
	jsonData, err := json.Marshal(report)
	if err != nil {
		fmt.Println("Error marshaling JSON:", err)
		return
	}

	// Send the report via POST request
	if reportURL != "" {
		resp, err := http.Post(reportURL, "application/json", bytes.NewBuffer(jsonData))
		if err != nil {
			fmt.Println("Error sending POST request:", err)
		} else {
			resp.Body.Close()
			fmt.Println("Current status reported")
		}
	}

	// Send the report to WebSocket clients
	if len(wsClients) != 0 {
		for client := range wsClients {
			err := client.WriteMessage(websocket.TextMessage, jsonData)
			if err != nil {
				log.Println("Error sending message to WebSocket client:", err)
				client.Close()
				delete(wsClients, client)
			}
		}
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

func convertToXYZ(c color.Color) (x, y, z float64) {
	// First convert c to colorful.Color
	cfColor, ok := colorful.MakeColor(c)
	if !ok {
		// Handle error case where conversion failed
		return
	}

	// Then use cfColor.Xyz() to get XYZ values
	x, y, z = cfColor.Xyz()
	return
}

func calculateContrastRatio(c1, c2 color.Color) float64 {
	// Convert colors to XYZ color space
	x1, y1, z1 := convertToXYZ(c1)
	x2, y2, z2 := convertToXYZ(c2)

	// Calculate relative luminance
	lum1 := 0.2126*x1 + 0.7152*y1 + 0.0722*z1
	lum2 := 0.2126*x2 + 0.7152*y2 + 0.0722*z2

	// Calculate contrast ratio
	if lum1 > lum2 {
		return (lum1 + 0.05) / (lum2 + 0.05)
	}
	return (lum2 + 0.05) / (lum1 + 0.05)
}

func provideTextColor(bgColor color.RGBA) color.RGBA {
	return chooseBestContrastingColor(bgColor, []color.RGBA{{0, 0, 0, 255}, {255, 255, 255, 255}})
}

func chooseBestContrastingColor(c color.RGBA, colors []color.RGBA) color.RGBA {
	var bestColor color.RGBA
	var bestContrast float64

	// Convert base color to colorful.Color
	baseColor := colorful.Color{R: float64(c.R) / 255, G: float64(c.G) / 255, B: float64(c.B) / 255}

	for _, col := range colors {
		// Convert color to colorful.Color
		contrastColor := colorful.Color{R: float64(col.R) / 255, G: float64(col.G) / 255, B: float64(col.B) / 255}

		// Calculate contrast ratio
		contrast := calculateContrastRatio(baseColor, contrastColor)

		// Update best color if contrast is higher
		if contrast > bestContrast {
			bestContrast = contrast
			bestColor = col
		}
	}

	return bestColor
}

func rgbToHSL(r, g, b uint8) (h, s, l float64) {
	c := colorful.Color{R: float64(r) / 255, G: float64(g) / 255, B: float64(b) / 255}
	h, s, l = c.Hsl()
	return
}

func hslToRGB(h, s, l float64) (r, g, b uint8) {
	c := colorful.Hsl(h, s, l)
	r, g, b = uint8(c.R*255), uint8(c.G*255), uint8(c.B*255)
	return
}

func provideAnalogousColors(c color.RGBA) (color.RGBA, color.RGBA) {
	// Convert RGBA to HSL
	h, s, l := rgbToHSL(c.R, c.G, c.B)

	// Calculate analogous hues by adding/subtracting 30 degrees
	h1 := math.Mod(h+30, 360)
	h2 := math.Mod(h-30, 360)

	// Convert HSL back to RGBA
	r1, g1, b1 := hslToRGB(h1, s, l)
	r2, g2, b2 := hslToRGB(h2, s, l)

	return color.RGBA{R: r1, G: g1, B: b1, A: c.A},
		color.RGBA{R: r2, G: g2, B: b2, A: c.A}
}

func provideTriadicColors(c color.RGBA) (color.RGBA, color.RGBA) {
	// Convert RGBA to HSL
	h, s, l := rgbToHSL(c.R, c.G, c.B)

	// Calculate triadic hues by adding/subtracting 120 degrees
	h1 := math.Mod(h+120, 360)
	h2 := math.Mod(h+240, 360)

	// Convert HSL back to RGBA
	r1, g1, b1 := hslToRGB(h1, s, l)
	r2, g2, b2 := hslToRGB(h2, s, l)

	return color.RGBA{R: r1, G: g1, B: b1, A: c.A},
		color.RGBA{R: r2, G: g2, B: b2, A: c.A}
}

func provideComplementaryColor(c color.RGBA) color.RGBA {
	// Convert RGBA to HSL
	h, s, l := rgbToHSL(c.R, c.G, c.B)

	// Calculate complementary hue by adding 180 degrees
	h = math.Mod(h+180, 360)

	// Convert HSL back to RGBA
	r, g, b := hslToRGB(h, s, l)

	return color.RGBA{R: r, G: g, B: b, A: c.A}
}

const playerTemplate string = `
<!DOCTYPE html>
<html>
	<head>
		<link rel="preconnect" href="https://fonts.googleapis.com">
		<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
		<link href="https://fonts.googleapis.com/css2?family=Montserrat:ital,wght@0,100..900;1,100..900&display=swap" rel="stylesheet">
		<style>
			body {
				background-color: transparent;
				font-family: "Montserrat", sans-serif;
				font-optical-sizing: auto;
				font-weight: 400;
				font-style: normal;
			}

			.player {
				display: flex;
				min-width: 500px;
				max-width: 500px;
				align-items: center;
				padding: 20px;
				background-color: rgba({{.AlbumArtColorRGB}}, 0.6);
				border-radius: 8px;
				box-shadow: 0 2px 4px rgba(0, 0, 0, 0.1);
			}

			.album-art {
				width: 100px;
				height: 100px;
				margin-right: 20px;
				flex-shrink: 0;
			}

			.album-art img {
				width: 100%;
				height: 100%;
				object-fit: cover;
				border-radius: 4px;
			}

			.track-info {
				flex: 1;
				min-width: 0;
				overflow: hidden;
			}

			.track-title {
				font-weight: 600;
				font-size: 22px;
				margin: 0;
				color: rgba({{.TextColorRGB}}, 1.0);
				overflow: hidden;
				white-space: nowrap;
			}

			.artist {
				font-size: 16px;
				color: rgba({{.TextColorRGB}}, 1.0);
				margin: 5px 0;
			}

			.progress-bar {
				height: 6px;
				background-color: rgba(200, 200, 200, 0.25);
				border-radius: 3px;
				margin: 10px 0;
			}

			.progress {
				height: 100%;
				background-color: rgba({{.ProgressColorRGB}}, 0.8);
				border-radius: 3px;
			}

			.duration {
				font-size: 14px;
				display: flex;
				justify-content: space-between;
				color: rgba({{.TextColorRGB}}, 1.0);
			}
		</style>
	</head>
	<body>
		<div class="player">
			<div class="album-art">
				<img src="{{.AlbumArtURL}}" alt="Album Art">
			</div>
			<div class="track-info">
				<h2 class="track-title">{{.Track}}</h2>
				<p class="artist">{{.Artist}}</p>
				<div class="progress-bar">
					<div class="progress" style="width: {{.ProgressPct}}%"></div>
				</div>
				<div class="duration">
					<span class="current-time">{{.ProgressStr}}</span>
					<span class="total-time">{{.DurationStr}}</span>
				</div>
			</div>
		</div>
		<script>
			var socket;
			var reconnectInterval = 5000; // Reconnect interval in milliseconds

			function connect() {
				socket = new WebSocket("{{.wsURL}}");

				socket.onopen = function (event) {
					console.log("WebSocket connected");
				};

				socket.onmessage = function (event) {
					var data = JSON.parse(event.data);
					document.querySelector(".album-art img").src = data.album_art_url;
					document.querySelector(".track-title").textContent = data.track;
					document.querySelector(".artist").textContent = data.artist;
					document.querySelector(".progress").style.width = data.progress_pct_str;
					document.querySelector(".current-time").textContent = data.progress_str;
					document.querySelector(".total-time").textContent = data.duration_str;
					document.querySelector(".player").style.backgroundColor = "rgba(" + data.album_art_color_rgb + ", 0.6)";
					document.querySelector(".track-title").style.color = "rgba(" + data.text_color_rgb + ", 1.0)";
					document.querySelector(".artist").style.color = "rgba(" + data.text_color_rgb + ", 1.0)";
					document.querySelector(".duration").style.color = "rgba(" + data.text_color_rgb + ", 1.0)";
					document.querySelector(".progress").style.backgroundColor = "rgba(" + data.progress_color_rgb + ", 1.0)";
				};

				socket.onclose = function (event) {
					console.log("WebSocket disconnected. Reconnecting in " + reconnectInterval + "ms...");
					setTimeout(connect, reconnectInterval);
				};

				socket.onerror = function (error) {
					console.error("WebSocket error: ", error);
				};
			}

			connect();
		</script>
	</body>
</html>
`
