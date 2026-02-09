package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

// Asset represents your investment data
type Asset struct {
	ID            int     `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Quantity      float64 `json:"quantity"`
	AvgPrice      float64 `json:"avgPrice"`
	CurrentPrice  float64 `json:"currentPrice"`
	PreviousClose float64 `json:"previousClose"` // NEW FIELD
}

// Yahoo Finance API Response Structure
type YahooResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				ChartPreviousClose float64 `json:"chartPreviousClose"` // NEW FIELD
			} `json:"meta"`
		} `json:"result"`
	} `json:"chart"`
}

func main() {
	// 1. Load .env
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: No .env file found.")
	}

	// 2. Connect to Supabase
	connStr := os.Getenv("DB_URL")
	if connStr == "" {
		log.Fatal("DB_URL not found in .env file")
	}

	conn, err := pgx.Connect(context.Background(), connStr)
	if err != nil {
		log.Fatal("Unable to connect to database: ", err)
	}
	defer conn.Close(context.Background())
	fmt.Println("Successfully connected to Supabase cloud!")

	// 3. Setup Router
	r := gin.Default()

	// CORS Middleware
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// --- ROUTES ---

	// POST: Save a new asset
	r.POST("/api/assets", func(c *gin.Context) {
		var input Asset
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Save avg_price as previous_close initially so daily gain starts at 0
		query := `INSERT INTO assets (name, asset_type, quantity, avg_price, current_price, previous_close) VALUES ($1, $2, $3, $4, $4, $4)`
		_, err := conn.Exec(context.Background(), query, input.Name, input.Type, input.Quantity, input.AvgPrice)

		if err != nil {
			fmt.Println("Insert Error:", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "Asset saved!"})
	})

	// GET: Fetch all assets (NOW INCLUDES PREVIOUS_CLOSE)
	r.GET("/api/assets", func(c *gin.Context) {
		rows, err := conn.Query(context.Background(), "SELECT id, name, asset_type, quantity, avg_price, current_price, previous_close FROM assets")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Query failed"})
			return
		}
		defer rows.Close()

		var assets []Asset
		for rows.Next() {
			var a Asset
			// Scan must match the SELECT order
			if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.Quantity, &a.AvgPrice, &a.CurrentPrice, &a.PreviousClose); err != nil {
				continue
			}
			assets = append(assets, a)
		}
		c.JSON(http.StatusOK, assets)
	})

	// POST: Trigger a Price Update
	r.POST("/api/update-prices", func(c *gin.Context) {
		rows, err := conn.Query(context.Background(), "SELECT id, name, asset_type FROM assets")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch assets"})
			return
		}
		defer rows.Close()

		type AssetShort struct {
			ID   int
			Name string
			Type string
		}
		var targets []AssetShort
		for rows.Next() {
			var t AssetShort
			rows.Scan(&t.ID, &t.Name, &t.Type)
			targets = append(targets, t)
		}

		updatedCount := 0
		for _, t := range targets {
			symbol := t.Name

			if strings.Contains(symbol, " ") || strings.Contains(strings.ToLower(symbol), "test") {
				fmt.Printf("Skipping invalid ticker: %s\n", symbol)
				continue
			}

			// Call custom function to get Price AND Previous Close
			price, prevClose, err := fetchLivePrice(symbol)

			if err != nil {
				fmt.Printf("Failed to fetch price for %s: %v\n", symbol, err)
				continue
			}

			// Update DB with BOTH values
			_, err = conn.Exec(context.Background(), "UPDATE assets SET current_price = $1, previous_close = $2 WHERE id = $3", price, prevClose, t.ID)
			if err == nil {
				fmt.Printf("Updated %s: %f (Prev: %f)\n", symbol, price, prevClose)
				updatedCount++
			}
		}

		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Updated %d assets", updatedCount)})
	})

	r.Run(":8080")
}

// --- NEW HELPER FUNCTION: Returns Price AND Previous Close ---
func fetchLivePrice(symbol string) (float64, float64, error) {
	url := fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?interval=1d", symbol)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, 0, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("yahoo returned status: %s", resp.Status)
	}

	var data YahooResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, 0, err
	}

	if len(data.Chart.Result) == 0 {
		return 0, 0, fmt.Errorf("no data found for symbol")
	}

	// Return Current Price AND Previous Close
	return data.Chart.Result[0].Meta.RegularMarketPrice, data.Chart.Result[0].Meta.ChartPreviousClose, nil
}
