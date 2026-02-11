package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"
)

// --- DATA STRUCTURES ---

type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type Asset struct {
	ID            int     `json:"id"`
	UserID        int     `json:"userId"` // Links asset to a user
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
	// 1. Load Env & Connect DB
	godotenv.Load()
	connStr := os.Getenv("DB_URL")
	if connStr == "" {
		log.Fatal("DB_URL not found")
	}

	dbPool, err := pgxpool.New(context.Background(), connStr)
	if err != nil {
		log.Fatal("DB Connection failed:", err)
	}
	defer dbPool.Close()

	r := gin.Default()

	// 2. CORS: Allow Frontend to send "X-User-ID" header
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, DELETE")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-User-ID")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// --- AUTH ROUTES ---

	// POST /api/register
	r.POST("/api/register", func(c *gin.Context) {
		var u User
		if err := c.ShouldBindJSON(&u); err != nil {
			c.JSON(400, gin.H{"error": "Invalid input"})
			return
		}

		// Encrypt password (bcrypt handles any string, even "123")
		hashedPwd, _ := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)

		var newID int
		// Insert into Users table
		err := dbPool.QueryRow(context.Background(),
			"INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id",
			u.Username, string(hashedPwd)).Scan(&newID)

		if err != nil {
			c.JSON(500, gin.H{"error": "Username likely taken"})
			return
		}

		c.JSON(200, gin.H{"message": "User created", "userId": newID, "username": u.Username})
	})

	// POST /api/login
	r.POST("/api/login", func(c *gin.Context) {
		var u User
		if err := c.ShouldBindJSON(&u); err != nil {
			c.JSON(400, gin.H{"error": "Invalid input"})
			return
		}

		var dbID int
		var dbHash string

		// Find user by name
		err := dbPool.QueryRow(context.Background(),
			"SELECT id, password_hash FROM users WHERE username=$1", u.Username).Scan(&dbID, &dbHash)

		if err == pgx.ErrNoRows {
			c.JSON(401, gin.H{"error": "User not found"})
			return
		}

		// Compare the "123" input with the encrypted hash in DB
		err = bcrypt.CompareHashAndPassword([]byte(dbHash), []byte(u.Password))
		if err != nil {
			c.JSON(401, gin.H{"error": "Wrong password"})
			return
		}

		c.JSON(200, gin.H{"message": "Login successful", "userId": dbID, "username": u.Username})
	})

	// --- ASSET ROUTES (Protected) ---

	// POST /api/assets (Add or Merge)
	r.POST("/api/assets", func(c *gin.Context) {
		// Identify User
		userIDStr := c.GetHeader("X-User-ID")
		if userIDStr == "" {
			c.JSON(401, gin.H{"error": "Unauthorized"})
			return
		}
		userID, _ := strconv.Atoi(userIDStr)

		var input Asset
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		var existingID int
		var existingQty float64
		var existingAvgPrice float64

		// Check if asset exists FOR THIS USER ONLY
		queryCheck := `SELECT id, COALESCE(quantity, 0), COALESCE(avg_price, 0) FROM assets WHERE name=$1 AND user_id=$2 LIMIT 1`
		err := dbPool.QueryRow(context.Background(), queryCheck, input.Name, userID).Scan(&existingID, &existingQty, &existingAvgPrice)

		if err == pgx.ErrNoRows {
			// New Asset
			currency := "USD"
			if strings.HasSuffix(input.Name, ".NS") || strings.HasSuffix(input.Name, ".BO") {
				currency = "INR"
			} else if strings.HasSuffix(input.Name, ".SI") {
				currency = "SGD"
			}

			insertQ := `INSERT INTO assets (user_id, name, asset_type, quantity, avg_price, current_price, previous_close, currency) VALUES ($1, $2, $3, $4, $5, $5, $5, $6)`
			_, err := dbPool.Exec(context.Background(), insertQ, userID, input.Name, input.Type, input.Quantity, input.AvgPrice, currency)

			if err != nil {
				c.JSON(500, gin.H{"error": "Failed to insert"})
				return
			}
			c.JSON(200, gin.H{"message": "Asset saved!"})

		} else {
			// Merge Asset
			newTotalQty := existingQty + input.Quantity
			var newAvgPrice float64
			if newTotalQty > 0 {
				newAvgPrice = ((existingQty * existingAvgPrice) + (input.Quantity * input.AvgPrice)) / newTotalQty
			}
			_, err = dbPool.Exec(context.Background(), "UPDATE assets SET quantity=$1, avg_price=$2 WHERE id=$3", newTotalQty, newAvgPrice, existingID)
			c.JSON(200, gin.H{"message": "Asset merged!"})
		}
	})

	// GET /api/assets (Fetch My Assets)
	r.GET("/api/assets", func(c *gin.Context) {
		userIDStr := c.GetHeader("X-User-ID")
		if userIDStr == "" {
			c.JSON(401, gin.H{"error": "Unauthorized"})
			return
		}
		userID, _ := strconv.Atoi(userIDStr)

		// Select only assets belonging to userID
		query := `SELECT id, name, asset_type, quantity, avg_price, current_price, previous_close, currency FROM assets WHERE user_id=$1 ORDER BY (current_price * quantity) DESC`
		rows, err := dbPool.Query(context.Background(), query, userID)
		if err != nil {
			c.JSON(500, gin.H{"error": "Query failed"})
			return
		}
		defer rows.Close()

		var assets []Asset
		for rows.Next() {
			var a Asset
			rows.Scan(&a.ID, &a.Name, &a.Type, &a.Quantity, &a.AvgPrice, &a.CurrentPrice, &a.PreviousClose, &a.Currency)
			assets = append(assets, a)
		}
		c.JSON(200, assets)
	})

	// DELETE /api/assets/:id
	r.DELETE("/api/assets/:id", func(c *gin.Context) {
		userIDStr := c.GetHeader("X-User-ID")
		id := c.Param("id")
		userID, _ := strconv.Atoi(userIDStr)

		// Secure Delete: Ensure ID matches AND User matches
		res, err := dbPool.Exec(context.Background(), "DELETE FROM assets WHERE id=$1 AND user_id=$2", id, userID)
		if err != nil || res.RowsAffected() == 0 {
			c.JSON(500, gin.H{"error": "Failed to delete"})
			return
		}
		c.JSON(200, gin.H{"message": "Deleted"})
	})

	// GLOBAL PRICE UPDATE (Updates everyone's stocks at once)
	r.POST("/api/update-prices", func(c *gin.Context) {
		rows, _ := dbPool.Query(context.Background(), "SELECT DISTINCT name FROM assets")
		var names []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			names = append(names, n)
		}
		rows.Close()

		for _, symbol := range names {
			price, prevClose, currency, err := fetchLivePriceExtended(symbol)
			if err == nil {
				dbPool.Exec(context.Background(), "UPDATE assets SET current_price=$1, previous_close=$2, currency=$3 WHERE name=$4", price, prevClose, currency, symbol)
			}
		}
		c.JSON(200, gin.H{"message": "Prices updated"})
	})

	// PUBLIC RATES
	r.GET("/api/rates", func(c *gin.Context) {
		inr, _, _, _ := fetchLivePriceExtended("INR=X")
		sgd, _, _, _ := fetchLivePriceExtended("SGD=X")
		if inr == 0 {
			inr = 87.0
		}
		if sgd == 0 {
			sgd = 1.36
		}
		c.JSON(200, gin.H{"USD": 1.0, "INR": inr, "SGD": sgd})
	})

	// SELF PING (Replace URL with yours)
	url := "https://YOUR-APP-NAME.onrender.com/api/rates"
	go func() {
		time.Sleep(1 * time.Minute)
		ticker := time.NewTicker(10 * time.Minute)
		for range ticker.C {
			http.Get(url)
		}
	}()

	r.Run(":8080")
}

func fetchLivePriceExtended(symbol string) (float64, float64, string, error) {
	url := fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?interval=1d", symbol)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, "", fmt.Errorf("bad status")
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
