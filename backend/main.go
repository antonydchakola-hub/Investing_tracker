package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq" // PostgreSQL driver
)

// Asset matches the JSON structure sent from your frontend
type Asset struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Quantity float64 `json:"quantity"`
	Price    float64 `json:"avgPrice"`
}

func main() {
	// 1. Load the .env file to get your DB_URL secret
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: No .env file found. Using system environment variables.")
	}

	// 2. Connect to Supabase using the URL from your .env file
	connStr := os.Getenv("DB_URL")
	if connStr == "" {
		log.Fatal("DB_URL not found in .env file or environment")
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Error opening database: ", err)
	}
	defer db.Close()

	// Verify the connection is active
	err = db.Ping()
	if err != nil {
		log.Fatal("Could not connect to Supabase: ", err)
	}
	fmt.Println("Successfully connected to Supabase cloud!")

	// 3. Setup Gin Router
	r := gin.Default()

	// 4. CORS Middleware
	// This allows your local HTML file to talk to this Go server
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// 5. STATUS ROUTE: Check if the backend is alive
	r.GET("/api/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "online",
			"db":     "connected",
		})
	})

	// 6. SAVE ASSET ROUTE: Receives data and saves to Supabase
	r.POST("/api/assets", func(c *gin.Context) {
		var incomingAsset Asset

		// Bind JSON from frontend to the struct
		if err := c.ShouldBindJSON(&incomingAsset); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid data format"})
			return
		}

		// SQL Query to insert into the 'assets' table you created
		query := `INSERT INTO assets (name, asset_type, quantity, avg_price) VALUES ($1, $2, $3, $4)`
		_, err := db.Exec(query, incomingAsset.Name, incomingAsset.Type, incomingAsset.Quantity, incomingAsset.Price)

		if err != nil {
			fmt.Println("Database Insert Error:", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save to database"})
			return
		}

		fmt.Printf("Saved to Cloud: %s (%v)\n", incomingAsset.Name, incomingAsset.Quantity)
		c.JSON(http.StatusOK, gin.H{"message": "Asset saved to Supabase!"})
	})

	// 7. Start the server
	fmt.Println("Go Backend is running on http://localhost:8080")
	r.Run(":8080")
}
