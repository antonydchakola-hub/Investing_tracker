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
	PreviousClose float64 `json:"previousClose"`
	Currency      string  `json:"currency"` // NEW FIELD
}

// Yahoo Finance API Response Structure
type YahooResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Currency           string  `json:"currency"` // Yahoo tells us if it's USD or INR
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				ChartPreviousClose float64 `json:"chartPreviousClose"`
			} `json:"meta"`
		} `json:"result"`
	} `json:"chart"`
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: No .env file found.")
	}

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

	r := gin.Default()

	// CORS
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

	// POST: Save a new asset (Default to USD, will update on refresh)
	r.POST("/api/assets", func(c *gin.Context) {
		var input Asset
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Determine currency based on input (Basic guess, refined later by Yahoo)
		currency := "USD"
		if strings.HasSuffix(input.Name, ".NS") || strings.HasSuffix(input.Name, ".BO") {
			currency = "INR"
		}

		query := `INSERT INTO assets (name, asset_type, quantity, avg_price, current_price, previous_close, currency) VALUES ($1, $2, $3, $4, $4, $4, $5)`
		_, err := conn.Exec(context.Background(), query, input.Name, input.Type, input.Quantity, input.AvgPrice, currency)

		if err != nil {
			fmt.Println("Insert Error:", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "Asset saved!"})
	})

	// GET: Fetch all assets
	r.GET("/api/assets", func(c *gin.Context) {
		rows, err := conn.Query(context.Background(), "SELECT id, name, asset_type, quantity, avg_price, current_price, previous_close, currency FROM assets")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Query failed"})
			return
		}
		defer rows.Close()

		var assets []Asset
		for rows.Next() {
			var a Asset
			if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.Quantity, &a.AvgPrice, &a.CurrentPrice, &a.PreviousClose, &a.Currency); err != nil {
				continue
			}
			assets = append(assets, a)
		}
		c.JSON(http.StatusOK, assets)
	})

	// GET: Fetch Exchange Rates (USD base)
	r.GET("/api/rates", func(c *gin.Context) {
		// We fetch INR=X (USD to INR) and SGD=X (USD to SGD)
		inrRate, _, _ := fetchLivePrice("INR=X")
		sgdRate, _, _ := fetchLivePrice("SGD=X")

		// If fetch fails, provide safe fallbacks
		if inrRate == 0 {
			inrRate = 83.0
		}
		if sgdRate == 0 {
			sgdRate = 1.35
		}

		c.JSON(http.StatusOK, gin.H{
			"USD": 1.0,
			"INR": inrRate,
			"SGD": sgdRate,
		})
	})

	// POST: Trigger Price Update
	r.POST("/api/update-prices", func(c *gin.Context) {
		rows, err := conn.Query(context.Background(), "SELECT id, name FROM assets")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch assets"})
			return
		}
		defer rows.Close()

		type AssetShort struct {
			ID   int
			Name string
		}
		var targets []AssetShort
		for rows.Next() {
			var t AssetShort
			rows.Scan(&t.ID, &t.Name)
			targets = append(targets, t)
		}

		updatedCount := 0
		for _, t := range targets {
			symbol := t.Name
			if strings.Contains(symbol, " ") || strings.Contains(strings.ToLower(symbol), "test") {
				continue
			}

			// Fetch Price, Close, AND Currency from Yahoo
			price, prevClose, currency, err := fetchLivePriceExtended(symbol)

			if err != nil {
				fmt.Printf("Failed: %s - %v\n", symbol, err)
				continue
			}

			// Update everything including detected currency
			_, err = conn.Exec(context.Background(), "UPDATE assets SET current_price=$1, previous_close=$2, currency=$3 WHERE id=$4", price, prevClose, currency, t.ID)
			if err == nil {
				fmt.Printf("Updated %s: %f %s\n", symbol, price, currency)
				updatedCount++
			}
		}

		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Updated %d assets", updatedCount)})
	})

	r.Run(":8080")
}

// Helper: Basic fetch (reused for rates)
func fetchLivePrice(symbol string) (float64, float64, error) {
	p, c, _, err := fetchLivePriceExtended(symbol)
	return p, c, err
}

// Helper: Detailed fetch including Currency
func fetchLivePriceExtended(symbol string) (float64, float64, string, error) {
	url := fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?interval=1d", symbol)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, 0, "", fmt.Errorf("status: %s", resp.Status)
	}

	var data YahooResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, 0, "", err
	}

	if len(data.Chart.Result) == 0 {
		return 0, 0, "", fmt.Errorf("no data")
	}

	meta := data.Chart.Result[0].Meta
	return meta.RegularMarketPrice, meta.ChartPreviousClose, meta.Currency, nil
}
