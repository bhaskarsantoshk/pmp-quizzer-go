package main

import (
    "database/sql"
    "encoding/json"
    "html/template"
    "log"
    "math/rand"
    "net/http"
    "os"
    "time"

    "github.com/gin-contrib/sessions"
    "github.com/gin-contrib/sessions/cookie"
    "github.com/gin-gonic/gin"
    "github.com/google/uuid"
    _ "github.com/mattn/go-sqlite3"
)

// Question represents a single PMP MCQ.
type Question struct {
    Text          string   `json:"question"`
    Options       []string `json:"options"`
    CorrectAnswer string   `json:"correct_answer"`
    Difficulty    string   `json:"difficulty"`
}

// AnsweredQuestion pairs a Question with the user’s selected answer.
type AnsweredQuestion struct {
    Question      Question `json:"question"`
    UserAnswer    string   `json:"user_answer"`
    CorrectAnswer string   `json:"correct_answer"`
    Difficulty    string   `json:"difficulty"`
}

var (
    allQuestions []Question
    db           *sql.DB
)

func main() {
    // (1) Load all 15 questions from JSON into allQuestions
    if err := loadQuestions("questions.json"); err != nil {
        log.Fatalf("Failed to load questions: %v", err)
    }

    // (2) Shuffle once at startup
    rand.Seed(time.Now().UnixNano())
    rand.Shuffle(len(allQuestions), func(i, j int) {
        allQuestions[i], allQuestions[j] = allQuestions[j], allQuestions[i]
    })

    // (3) Open in-memory SQLite DB
    var err error
    db, err = sql.Open("sqlite3", ":memory:")
    if err != nil {
        log.Fatalf("Cannot open in-memory sqlite: %v", err)
    }
    defer db.Close()

    // (4) Create tables: quiz_state and answers
    createSchema := `
    CREATE TABLE quiz_state (
        quiz_id      TEXT PRIMARY KEY,
        current_index INTEGER
    );
    CREATE TABLE answers (
        quiz_id        TEXT,
        question_index INTEGER,
        user_answer    TEXT,
        correct_answer TEXT,
        difficulty     TEXT
    );
    `
    if _, err := db.Exec(createSchema); err != nil {
        log.Fatalf("Failed to create schema: %v", err)
    }

    // (5) Set up Gin
    gin.SetMode(gin.ReleaseMode)
    router := gin.Default()

    // Allow {{add .CurrentIndex 1}} in templates
    router.SetFuncMap(template.FuncMap{
        "add": func(a, b int) int { return a + b },
    })

    // We'll still use Gin’s cookie‐based sessions, but only to store our own "quiz_id" cookie.
    store := cookie.NewStore([]byte("super-secret-key"))
    router.Use(sessions.Sessions("pmp_session", store))

    // Load our HTML templates
    router.LoadHTMLGlob("templates/*")

    // Routes
    router.GET("/", startExamHandler)    // Generate quiz_id & redirect to /q
    router.GET("/q", questionHandler)    // Show current question by reading session’s quiz_id
    router.POST("/next", nextHandler)     // Save answer & redirect to /q
    router.POST("/end", endHandler)       // Save answer & render summary
    router.GET("/end", endHandler)        // Summary if time expires on the last question

    // Launch
    router.Run(":8080")
}

// loadQuestions reads the entire questions.json into allQuestions
func loadQuestions(filename string) error {
    f, err := os.Open(filename)
    if err != nil {
        return err
    }
    defer f.Close()
    decoder := json.NewDecoder(f)
    return decoder.Decode(&allQuestions)
}

// startExamHandler: always generate a new quiz_id, initialize DB, redirect to /q
func startExamHandler(c *gin.Context) {
    session := sessions.Default(c)

    // Always create a fresh quiz_id
    newID := uuid.NewString()
    session.Set("quiz_id", newID)
    session.Save()

    // Insert initial state for this new quiz
    _, err := db.Exec(`INSERT INTO quiz_state(quiz_id, current_index) VALUES (?, ?)`, newID, 0)
    if err != nil {
        log.Fatalf("Failed to insert quiz_state: %v", err)
    }

    // Redirect to /q (question page)
    c.Redirect(http.StatusSeeOther, "/q")
}

// questionHandler: read quiz_id from session, fetch current_index from DB, and render that question
func questionHandler(c *gin.Context) {
    session := sessions.Default(c)
    quizIDraw := session.Get("quiz_id")
    if quizIDraw == nil {
        c.Redirect(http.StatusSeeOther, "/")
        return
    }
    quizID := quizIDraw.(string)

    // Fetch current_index from quiz_state
    var currentIndex int
    err := db.QueryRow(`SELECT current_index FROM quiz_state WHERE quiz_id = ?`, quizID).Scan(&currentIndex)
    if err != nil {
        log.Printf("No quiz_state for %s, redirecting to /", quizID)
        c.Redirect(http.StatusSeeOther, "/")
        return
    }

    // If currentIndex >= len(allQuestions), go straight to summary
    if currentIndex >= len(allQuestions) {
        c.Redirect(http.StatusSeeOther, "/end")
        return
    }

    // Render the question with index = currentIndex
    c.HTML(http.StatusOK, "exam.html", gin.H{
        "Question":     allQuestions[currentIndex],
        "CurrentIndex": currentIndex,
        "TotalCount":   len(allQuestions),
    })
}

// nextHandler: save answer for current question, increment index in DB, then redirect to /q
func nextHandler(c *gin.Context) {
    session := sessions.Default(c)
    quizIDraw := session.Get("quiz_id")
    if quizIDraw == nil {
        c.Redirect(http.StatusSeeOther, "/")
        return
    }
    quizID := quizIDraw.(string)

    // 1) Fetch current_index from DB
    var currentIndex int
    err := db.QueryRow(`SELECT current_index FROM quiz_state WHERE quiz_id = ?`, quizID).Scan(&currentIndex)
    if err != nil {
        log.Printf("nextHandler: no quiz_state for %s, redirecting to /", quizID)
        c.Redirect(http.StatusSeeOther, "/")
        return
    }
    log.Printf("nextHandler: currentIndex=%d (DB) for quiz_id=%s", currentIndex, quizID)

    // 2) Append this question’s answer into the "answers" table
    q := allQuestions[currentIndex]
    selected := c.PostForm("answer") // might be "" if timer expired
    log.Printf("nextHandler: question=%q userAnswer=%q correct=%q", q.Text, selected, q.CorrectAnswer)

    _, err = db.Exec(
        `INSERT INTO answers(quiz_id, question_index, user_answer, correct_answer, difficulty)
         VALUES (?, ?, ?, ?, ?)`,
        quizID, currentIndex, selected, q.CorrectAnswer, q.Difficulty,
    )
    if err != nil {
        log.Fatalf("Failed to insert answer: %v", err)
    }

    // 3) Increment current_index in quiz_state
    nextIndex := currentIndex + 1
    _, err = db.Exec(`UPDATE quiz_state SET current_index = ? WHERE quiz_id = ?`, nextIndex, quizID)
    if err != nil {
        log.Fatalf("Failed to update quiz_state: %v", err)
    }
    log.Printf("nextHandler: advanced to currentIndex=%d (DB)", nextIndex)

    // 4) If nextIndex >= total questions, redirect to summary
    if nextIndex >= len(allQuestions) {
        c.Redirect(http.StatusSeeOther, "/end")
        return
    }

    // 5) Otherwise, redirect to /q (which will fetch the new index from DB)
    c.Redirect(http.StatusSeeOther, "/q")
}

// endHandler: if POST, capture final answer; then collect all answers from DB and render summary
func endHandler(c *gin.Context) {
    session := sessions.Default(c)
    quizIDraw := session.Get("quiz_id")
    if quizIDraw == nil {
        c.Redirect(http.StatusSeeOther, "/")
        return
    }
    quizID := quizIDraw.(string)

    // 1) Fetch current_index from DB
    var currentIndex int
    err := db.QueryRow(`SELECT current_index FROM quiz_state WHERE quiz_id = ?`, quizID).Scan(&currentIndex)
    if err != nil {
        log.Printf("endHandler: no quiz_state for %s", quizID)
        c.Redirect(http.StatusSeeOther, "/")
        return
    }

    // 2) If this is a POST, capture final question’s answer
    if c.Request.Method == http.MethodPost {
        if currentIndex < len(allQuestions) {
            q := allQuestions[currentIndex]
            selected := c.PostForm("answer")
            log.Printf("endHandler (POST): question=%q userAnswer=%q correct=%q", q.Text, selected, q.CorrectAnswer)

            _, err := db.Exec(
                `INSERT INTO answers(quiz_id, question_index, user_answer, correct_answer, difficulty)
                 VALUES (?, ?, ?, ?, ?)`,
                quizID, currentIndex, selected, q.CorrectAnswer, q.Difficulty,
            )
            if err != nil {
                log.Fatalf("Failed to insert final answer: %v", err)
            }
            // Advance index beyond last
            _, err = db.Exec(`UPDATE quiz_state SET current_index = ? WHERE quiz_id = ?`, currentIndex+1, quizID)
            if err != nil {
                log.Fatalf("Failed to advance index in quiz_state: %v", err)
            }
        }
    } else {
        log.Printf("endHandler (GET): just rendering summary for quiz_id=%s", quizID)
    }

    // 3) Query all answers for this quiz_id
    rows, err := db.Query(
        `SELECT question_index, user_answer, correct_answer, difficulty
         FROM answers WHERE quiz_id = ? ORDER BY question_index ASC`, quizID,
    )
    if err != nil {
        log.Fatalf("Failed to query answers: %v", err)
    }
    defer rows.Close()

    var answered []AnsweredQuestion
    for rows.Next() {
        var qi int
        var ua, ca, diff string
        if err := rows.Scan(&qi, &ua, &ca, &diff); err != nil {
            log.Fatalf("Row scan failed: %v", err)
        }
        // Rehydrate the question text from allQuestions[qi]
        q := allQuestions[qi]
        answered = append(answered, AnsweredQuestion{
            Question:      q,
            UserAnswer:    ua,
            CorrectAnswer: ca,
            Difficulty:    diff,
        })
    }

    // 4) Compute correct count
    correctCount := 0
    for _, a := range answered {
        if a.UserAnswer == a.CorrectAnswer {
            correctCount++
        }
    }

    // 5) Render summary.html with all answers
    c.HTML(http.StatusOK, "summary.html", gin.H{
        "Answered": answered,
        "Correct":  correctCount,
        "Total":    len(answered),
    })
}