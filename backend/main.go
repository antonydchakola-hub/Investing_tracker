package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// Data Structures
type Asset struct {
	ID            int     `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Quantity      float64 `json:"quantity"`
	AvgPrice      float64 `json:"avgPrice"`
	CurrentPrice  float64 `json:"currentPrice"`
	PreviousClose float64 `json:"previousClose"`
	Currency      string  `json:"currency"`
}

type YahooResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Currency           string  `json:"currency"`
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				ChartPreviousClose float64 `json:"chartPreviousClose"`
			} `json:"meta"`
		} `json:"result"`
	} `json:"chart"`
}

func main() {
	// 1. Load Environment Variables
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: No .env file found (this is fine on Render).")
	}

	connStr := os.Getenv("DB_URL")
	if connStr == "" {
		log.Fatal("DB_URL not found in environment variables")
	}

	// 2. Connect to Database (Using Pool for concurrency)
	dbPool, err := pgxpool.New(context.Background(), connStr)
	if err != nil {
		log.Fatal("Unable to connect to database: ", err)
	}
	defer dbPool.Close()

	fmt.Println("Successfully connected to Supabase (Pool Mode)!")

	// 3. Setup Router
	r := gin.Default()

	// CORS Setup (Allow Frontend to talk to Backend)
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, DELETE")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// --- ROUTES ---

	// POST: Save OR Merge Asset
	r.POST("/api/assets", func(c *gin.Context) {
		var input Asset
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		var existingID int
		var existingQty float64
		var existingAvgPrice float64

		// Check if asset already exists
		queryCheck := `SELECT id, COALESCE(quantity, 0), COALESCE(avg_price, 0) FROM assets WHERE name=$1 LIMIT 1`
		err := dbPool.QueryRow(context.Background(), queryCheck, input.Name).Scan(&existingID, &existingQty, &existingAvgPrice)

		if err == pgx.ErrNoRows {
			// New Asset Logic
			currency := "USD"
			if strings.HasSuffix(input.Name, ".NS") || strings.HasSuffix(input.Name, ".BO") {
				currency = "INR"
			} else if strings.HasSuffix(input.Name, ".SI") {
				currency = "SGD"
			}

			insertQ := `INSERT INTO assets (name, asset_type, quantity, avg_price, current_price, previous_close, currency) VALUES ($1, $2, $3, $4, $4, $4, $5)`
			_, err := dbPool.Exec(context.Background(), insertQ, input.Name, input.Type, input.Quantity, input.AvgPrice, currency)

			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to insert"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"message": "Asset saved!"})

		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return

		} else {
			// Merge Logic (Weighted Average)
			newTotalQty := existingQty + input.Quantity
			var newAvgPrice float64
			if newTotalQty > 0 {
				totalCost := (existingQty * existingAvgPrice) + (input.Quantity * input.AvgPrice)
				newAvgPrice = totalCost / newTotalQty
			} else {
				newAvgPrice = 0
			}

			_, err = dbPool.Exec(context.Background(), "UPDATE assets SET quantity=$1, avg_price=$2 WHERE id=$3", newTotalQty, newAvgPrice, existingID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"message": "Asset merged!"})
		}
	})

	// GET: Fetch all assets
	r.GET("/api/assets", func(c *gin.Context) {
		query := `
			SELECT 
				id, name, asset_type, 
				COALESCE(quantity, 0), 
				COALESCE(avg_price, 0), 
				COALESCE(current_price, 0), 
				COALESCE(previous_close, 0), 
				COALESCE(currency, 'USD') 
			FROM assets ORDER BY (current_price * quantity) DESC`

		rows, err := dbPool.Query(context.Background(), query)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Query failed"})
			return
		}
		defer rows.Close()

		var assets []Asset
		for rows.Next() {
			var a Asset
			err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.Quantity, &a.AvgPrice, &a.CurrentPrice, &a.PreviousClose, &a.Currency)
			if err != nil {
				continue
			}
			assets = append(assets, a)
		}
		c.JSON(http.StatusOK, assets)
	})

	// GET: Exchange Rates
	r.GET("/api/rates", func(c *gin.Context) {
		inrRate, _, _ := fetchLivePrice("INR=X")
		sgdRate, _, _ := fetchLivePrice("SGD=X")

		// Fallbacks if Yahoo blocks us
		if inrRate == 0 {
			inrRate = 87.0
		}
		if sgdRate == 0 {
			sgdRate = 1.36
		}

		c.JSON(http.StatusOK, gin.H{"USD": 1.0, "INR": inrRate, "SGD": sgdRate})
	})

	// POST: Update Prices (Scraper)
	r.POST("/api/update-prices", func(c *gin.Context) {
		rows, err := dbPool.Query(context.Background(), "SELECT id, name FROM assets")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch assets"})
			return
		}

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
		rows.Close()

		updatedCount := 0
		for _, t := range targets {
			symbol := t.Name
			// Skip testing symbols
			if strings.Contains(symbol, " ") || strings.Contains(strings.ToLower(symbol), "test") {
				continue
			}

			price, prevClose, currency, err := fetchLivePriceExtended(symbol)
			if err != nil {
				continue
			}

			// Update DB
			_, err = dbPool.Exec(context.Background(), "UPDATE assets SET current_price=$1, previous_close=$2, currency=$3 WHERE id=$4", price, prevClose, currency, t.ID)
			if err == nil {
				updatedCount++
			}
		}

		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Updated %d assets", updatedCount)})
	})

	// --- NEW: DELETE ROUTE ---
	r.DELETE("/api/assets/:id", func(c *gin.Context) {
		id := c.Param("id")

		// Delete from database
		_, err := dbPool.Exec(context.Background(), "DELETE FROM assets WHERE id=$1", id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Asset deleted"})
	})

	// --- KEEP-ALIVE (SELF PING) ---
	// REPLACE THIS URL WITH YOUR ACTUAL RENDER BACKEND URL
	// Example: "https://investing-tracker.onrender.com/api/rates"
	url := "https://YOUR-APP-NAME.onrender.com/api/rates"

	go func() {
		time.Sleep(1 * time.Minute) // Wait for server to boot
		ticker := time.NewTicker(10 * time.Minute)
		for range ticker.C {
			resp, err := http.Get(url)
			if err != nil {
				fmt.Println("Keep-alive ping failed:", err)
			} else {
				fmt.Println("Keep-alive ping success! Status:", resp.StatusCode)
				resp.Body.Close()
			}
		}
	}()

	r.Run(":8080")
}

// Helpers
func fetchLivePrice(symbol string) (float64, float64, error) {
	p, c, _, err := fetchLivePriceExtended(symbol)
	return p, c, err
}

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
