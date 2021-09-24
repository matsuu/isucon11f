package main

import (
	"archive/zip"
	"compress/flate"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-sql-driver/mysql"
	"github.com/goccy/go-json"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/zhangyunhao116/skipmap"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sync/singleflight"
)

const (
	SQLDirectory              = "../sql/"
	AssignmentsDirectory      = "../assignments/"
	InitDataDirectory         = "../data/"
	SessionName               = "isucholar_go"
	mysqlErrNumDuplicateEntry = 1062
)

type handlers struct {
	DB *sqlx.DB
}

var (
	mapSubmitters *skipmap.StringMap
	gpasGroup     singleflight.Group
)

func main() {
	e := echo.New()
	e.Debug = GetEnv("DEBUG", "") == "true"
	sock := "/run/webapp/isucholar.sock"
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		e.Logger.Fatal(err)
	}
	e.Listener = l
	os.Chmod(sock, 0666)
	e.HideBanner = true

	e.Use(middleware.Recover())
	e.Use(session.Middleware(sessions.NewCookieStore([]byte("trapnomura"))))

	db, _ := GetDB(false)
	db.SetMaxOpenConns(500)
	db.SetMaxIdleConns(500)

	h := &handlers{
		DB: db,
	}

	if err := h.updateSet(); err != nil {
		panic(err)
	}

	e.POST("/initialize", h.Initialize)

	e.POST("/login", h.Login)
	e.POST("/logout", h.Logout)
	API := e.Group("/api", h.IsLoggedIn)
	{
		usersAPI := API.Group("/users")
		{
			usersAPI.GET("/me", h.GetMe)
			usersAPI.GET("/me/courses", h.GetRegisteredCourses)
			usersAPI.PUT("/me/courses", h.RegisterCourses)
			usersAPI.GET("/me/grades", h.GetGrades)
		}
		coursesAPI := API.Group("/courses")
		{
			coursesAPI.GET("", h.SearchCourses)
			coursesAPI.POST("", h.AddCourse, h.IsAdmin)
			coursesAPI.GET("/:courseID", h.GetCourseDetail)
			coursesAPI.PUT("/:courseID/status", h.SetCourseStatus, h.IsAdmin)
			coursesAPI.GET("/:courseID/classes", h.GetClasses)
			coursesAPI.POST("/:courseID/classes", h.AddClass, h.IsAdmin)
			coursesAPI.POST("/:courseID/classes/:classID/assignments", h.SubmitAssignment)
			coursesAPI.PUT("/:courseID/classes/:classID/assignments/scores", h.RegisterScores, h.IsAdmin)
			coursesAPI.GET("/:courseID/classes/:classID/assignments/export", h.DownloadSubmittedAssignments, h.IsAdmin)
		}
		announcementsAPI := API.Group("/announcements")
		{
			announcementsAPI.GET("", h.GetAnnouncementList)
			announcementsAPI.POST("", h.AddAnnouncement, h.IsAdmin)
			announcementsAPI.GET("/:announcementID", h.GetAnnouncementDetail)
		}
	}

	e.Logger.Error(e.StartServer(e.Server))
}

type InitializeResponse struct {
	Language string `json:"language"`
}

// Initialize POST /initialize 初期化エンドポイント
func (h *handlers) Initialize(c echo.Context) error {
	c.Request().ParseMultipartForm(65536)
	dbForInit, _ := GetDB(true)

	files := []string{
		"1_schema.sql",
		"2_init.sql",
		"3_sample.sql",
	}
	for _, file := range files {
		data, err := os.ReadFile(SQLDirectory + file)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		if _, err := dbForInit.Exec(string(data)); err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	if err := h.updateSet(); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if err := exec.Command("rm", "-rf", AssignmentsDirectory).Run(); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	err := filepath.WalkDir(InitDataDirectory, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		prefix := strings.TrimSuffix(d.Name(), ".pdf")
		ss := strings.Split(prefix, "-")
		classID, userID := ss[0], ss[1]

		var code, filename string
		if err := dbForInit.QueryRowx("SELECT code, file_name FROM submissions JOIN users ON submissions.user_id = users.id WHERE class_id = ? AND user_id = ?", classID, userID).Scan(&code, &filename); err != nil {
			return err
		}

		outdir := filepath.Join(AssignmentsDirectory, classID)
		if err := os.MkdirAll(outdir, 0755); err != nil {
			return err
		}
		outfile := filepath.Join(outdir, code+"-"+filename)
		return exec.Command("ln", path, outfile).Run()
	})
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if _, err := dbForInit.Exec("DELETE FROM unread_announcements WHERE is_deleted"); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if _, err := dbForInit.Exec("DROP TABLE IF EXISTS user_scores"); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if _, err := dbForInit.Exec("CREATE TABLE user_scores (id CHAR(26) PRIMARY KEY, score INT UNSIGNED NOT NULL DEFAULT 0, credit INT UNSIGNED NOT NULL DEFAULT 0)"); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if _, err := dbForInit.Exec("DROP TABLE IF EXISTS total_scores"); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if _, err := dbForInit.Exec("CREATE TABLE total_scores (user_id CHAR(26) NOT NULL, course_id CHAR(26) NOT NULL, score INT UNSIGNED NOT NULL DEFAULT 0, PRIMARY KEY(user_id, course_id))"); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	res := InitializeResponse{
		Language: "go",
	}
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return json.NewEncoder(c.Response()).Encode(res)
}

// IsLoggedIn ログイン確認用middleware
func (h *handlers) IsLoggedIn(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		sess, err := session.Get(SessionName, c)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		if sess.IsNew {
			return c.String(http.StatusUnauthorized, "You are not logged in.")
		}
		_, ok := sess.Values["userID"]
		if !ok {
			return c.String(http.StatusUnauthorized, "You are not logged in.")
		}

		return next(c)
	}
}

// IsAdmin admin確認用middleware
func (h *handlers) IsAdmin(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		sess, err := session.Get(SessionName, c)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		isAdmin, ok := sess.Values["isAdmin"]
		if !ok {
			c.Logger().Error("failed to get isAdmin from session")
			return c.NoContent(http.StatusInternalServerError)
		}
		if !isAdmin.(bool) {
			return c.String(http.StatusForbidden, "You are not admin user.")
		}

		return next(c)
	}
}

func getUserInfo(c echo.Context) (userID string, userName string, isAdmin bool, err error) {
	sess, err := session.Get(SessionName, c)
	if err != nil {
		return "", "", false, err
	}
	_userID, ok := sess.Values["userID"]
	if !ok {
		return "", "", false, fmt.Errorf("failed to get userID from session")
	}
	_userName, ok := sess.Values["userName"]
	if !ok {
		return "", "", false, fmt.Errorf("failed to get userName from session")
	}
	_isAdmin, ok := sess.Values["isAdmin"]
	if !ok {
		return "", "", false, fmt.Errorf("failed to get isAdmin from session")
	}
	return _userID.(string), _userName.(string), _isAdmin.(bool), nil
}

type UserType string

const (
	Student UserType = "student"
	Teacher UserType = "teacher"
)

type User struct {
	ID             string   `db:"id"`
	Code           string   `db:"code"`
	Name           string   `db:"name"`
	HashedPassword []byte   `db:"hashed_password"`
	Type           UserType `db:"type"`
}

type CourseType string

const (
	LiberalArts   CourseType = "liberal-arts"
	MajorSubjects CourseType = "major-subjects"
)

type DayOfWeek string

const (
	Monday    DayOfWeek = "monday"
	Tuesday   DayOfWeek = "tuesday"
	Wednesday DayOfWeek = "wednesday"
	Thursday  DayOfWeek = "thursday"
	Friday    DayOfWeek = "friday"
)

var daysOfWeek = []DayOfWeek{Monday, Tuesday, Wednesday, Thursday, Friday}

type CourseStatus string

const (
	StatusRegistration CourseStatus = "registration"
	StatusInProgress   CourseStatus = "in-progress"
	StatusClosed       CourseStatus = "closed"
)

type Course struct {
	ID          string       `db:"id"`
	Code        string       `db:"code"`
	Type        CourseType   `db:"type"`
	Name        string       `db:"name"`
	Description string       `db:"description"`
	Credit      uint8        `db:"credit"`
	Period      uint8        `db:"period"`
	DayOfWeek   DayOfWeek    `db:"day_of_week"`
	TeacherID   string       `db:"teacher_id"`
	Keywords    string       `db:"keywords"`
	Status      CourseStatus `db:"status"`
}

type Registration struct {
	CourseID string `db:"course_id"`
	UserID   string `db:"user_id"`
}

func (h *handlers) updateSet() error {
	mapSubmitters = skipmap.NewString()
	rows, err := h.DB.Queryx("SELECT class_id, COUNT(class_id) FROM submissions GROUP BY class_id")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var cnt int64
		err := rows.Scan(&id, &cnt)
		if err != nil {
			return err
		}
		mapSubmitters.Store(id, &cnt)
	}
	return nil
}

// ---------- Public API ----------

type LoginRequest struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}

// Login POST /login ログイン
func (h *handlers) Login(c echo.Context) error {
	var req LoginRequest
	if err := c.Bind(&req); err != nil {
		return c.String(http.StatusBadRequest, "Invalid format.")
	}

	var user User
	if err := h.DB.Get(&user, "SELECT * FROM `users` WHERE `code` = ?", req.Code); err != nil && err != sql.ErrNoRows {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	} else if err == sql.ErrNoRows {
		return c.String(http.StatusUnauthorized, "Code or Password is wrong.")
	}

	if bcrypt.CompareHashAndPassword(user.HashedPassword, []byte(req.Password)) != nil {
		return c.String(http.StatusUnauthorized, "Code or Password is wrong.")
	}

	sess, err := session.Get(SessionName, c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if userID, ok := sess.Values["userID"].(string); ok && userID == user.ID {
		return c.String(http.StatusBadRequest, "You are already logged in.")
	}

	sess.Values["userID"] = user.ID
	sess.Values["userName"] = user.Name
	sess.Values["isAdmin"] = user.Type == Teacher
	sess.Options = &sessions.Options{
		Path:   "/",
		MaxAge: 3600,
	}

	if err := sess.Save(c.Request(), c.Response()); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// Logout POST /logout ログアウト
func (h *handlers) Logout(c echo.Context) error {
	sess, err := session.Get(SessionName, c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	sess.Options = &sessions.Options{
		Path:   "/",
		MaxAge: -1,
	}

	if err := sess.Save(c.Request(), c.Response()); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// ---------- Users API ----------

type GetMeResponse struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"is_admin"`
}

// GetMe GET /api/users/me 自身の情報を取得
func (h *handlers) GetMe(c echo.Context) error {
	userID, userName, isAdmin, err := getUserInfo(c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var userCode string
	if err := h.DB.Get(&userCode, "SELECT `code` FROM `users` WHERE `id` = ?", userID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return json.NewEncoder(c.Response()).Encode(GetMeResponse{
		Code:    userCode,
		Name:    userName,
		IsAdmin: isAdmin,
	})
}

type GetRegisteredCourseResponseContent struct {
	ID        string    `db:"id" json:"id"`
	Name      string    `db:"name" json:"name"`
	Teacher   string    `db:"teacher" json:"teacher"`
	Period    uint8     `db:"period" json:"period"`
	DayOfWeek DayOfWeek `db:"day_of_week" json:"day_of_week"`
}

// GetRegisteredCourses GET /api/users/me/courses 履修中の科目一覧取得
func (h *handlers) GetRegisteredCourses(c echo.Context) error {
	userID, _, _, err := getUserInfo(c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var res []GetRegisteredCourseResponseContent
	query := "SELECT `courses`.id, courses.name, users.name AS teacher, courses.period, courses.day_of_week" +
		" FROM `courses`" +
		" JOIN `registrations` ON `courses`.`id` = `registrations`.`course_id`" +
		" JOIN users ON courses.teacher_id = users.id" +
		" WHERE `courses`.`status` != ? AND `registrations`.`user_id` = ?"
	if err := h.DB.Select(&res, query, StatusClosed, userID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return json.NewEncoder(c.Response()).Encode(res)
}

type RegisterCourseRequestContent struct {
	ID string `json:"id"`
}

type RegisterCoursesErrorResponse struct {
	CourseNotFound       []string `json:"course_not_found,omitempty"`
	NotRegistrableStatus []string `json:"not_registrable_status,omitempty"`
	ScheduleConflict     []string `json:"schedule_conflict,omitempty"`
}

// RegisterCourses PUT /api/users/me/courses 履修登録
func (h *handlers) RegisterCourses(c echo.Context) error {
	userID, _, _, err := getUserInfo(c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var req []RegisterCourseRequestContent
	if err := c.Bind(&req); err != nil {
		return c.String(http.StatusBadRequest, "Invalid format.")
	}
	sort.Slice(req, func(i, j int) bool {
		return req[i].ID < req[j].ID
	})

	tx, err := h.DB.Beginx()
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	var errors RegisterCoursesErrorResponse
	var newlyAdded []Course
	for _, courseReq := range req {
		courseID := courseReq.ID
		var course Course
		if err := tx.Get(&course, "SELECT * FROM `courses` WHERE `id` = ? FOR SHARE", courseID); err != nil && err != sql.ErrNoRows {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		} else if err == sql.ErrNoRows {
			errors.CourseNotFound = append(errors.CourseNotFound, courseReq.ID)
			continue
		}

		if course.Status != StatusRegistration {
			errors.NotRegistrableStatus = append(errors.NotRegistrableStatus, course.ID)
			continue
		}

		// すでに履修登録済みの科目は無視する
		var exists bool
		if err := tx.Get(&exists, "SELECT EXISTS(SELECT 1 FROM `registrations` WHERE `course_id` = ? AND `user_id` = ?)", course.ID, userID); err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		if exists {
			continue
		}

		newlyAdded = append(newlyAdded, course)
	}

	var alreadyRegistered []Course
	query := "SELECT `courses`.*" +
		" FROM `courses`" +
		" JOIN `registrations` ON `courses`.`id` = `registrations`.`course_id`" +
		" WHERE `courses`.`status` != ? AND `registrations`.`user_id` = ?"
	if err := tx.Select(&alreadyRegistered, query, StatusClosed, userID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	registrations := make([]Registration, 0, len(newlyAdded))
	alreadyRegistered = append(alreadyRegistered, newlyAdded...)
	for _, course1 := range newlyAdded {
		for _, course2 := range alreadyRegistered {
			if course1.ID != course2.ID && course1.Period == course2.Period && course1.DayOfWeek == course2.DayOfWeek {
				errors.ScheduleConflict = append(errors.ScheduleConflict, course1.ID)
				break
			}
		}
		registrations = append(registrations, Registration{
			CourseID: course1.ID,
			UserID:   userID,
		})
	}

	if len(errors.CourseNotFound) > 0 || len(errors.NotRegistrableStatus) > 0 || len(errors.ScheduleConflict) > 0 {
		return c.JSON(http.StatusBadRequest, errors)
	}

	_, err = tx.NamedExec("INSERT INTO `registrations` (`course_id`, `user_id`) VALUES (:course_id, :user_id) ON DUPLICATE KEY UPDATE `course_id` = VALUES(`course_id`), `user_id` = VALUES(`user_id`)", registrations)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if err = tx.Commit(); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

type Class struct {
	ID               string `db:"id"`
	CourseID         string `db:"course_id"`
	Part             uint8  `db:"part"`
	Title            string `db:"title"`
	Description      string `db:"description"`
	SubmissionClosed bool   `db:"submission_closed"`
}

type GetGradeResponse struct {
	Summary       Summary        `json:"summary"`
	CourseResults []CourseResult `json:"courses"`
}

type Gpas struct {
	Gpas   []float64
	GpaAvg float64
	GpaMax float64
	GpaMin float64
}

type Totals struct {
	Scores   []int
	ScoreAvg float64
	ScoreMax int
	ScoreMin int
}

type Summary struct {
	Credits   int     `json:"credits"`
	GPA       float64 `json:"gpa"`
	GpaTScore float64 `json:"gpa_t_score"` // 偏差値
	GpaAvg    float64 `json:"gpa_avg"`     // 平均値
	GpaMax    float64 `json:"gpa_max"`     // 最大値
	GpaMin    float64 `json:"gpa_min"`     // 最小値
}

type CourseResult struct {
	Name             string       `json:"name"`
	Code             string       `json:"code"`
	TotalScore       int          `json:"total_score"`
	TotalScoreTScore float64      `json:"total_score_t_score"` // 偏差値
	TotalScoreAvg    float64      `json:"total_score_avg"`     // 平均値
	TotalScoreMax    int          `json:"total_score_max"`     // 最大値
	TotalScoreMin    int          `json:"total_score_min"`     // 最小値
	ClassScores      []ClassScore `json:"class_scores"`
}

type ClassScore struct {
	ClassID    string `json:"class_id"`
	Title      string `json:"title"`
	Part       uint8  `json:"part"`
	Score      *int   `json:"score"`      // 0~100点
	Submitters int    `json:"submitters"` // 提出した学生数
}

// GetGrades GET /api/users/me/grades 成績取得
func (h *handlers) GetGrades(c echo.Context) error {
	userID, _, _, err := getUserInfo(c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// GPAの統計値
	// 一つでも修了した科目がある学生のGPA一覧
	ch := gpasGroup.DoChan("gpas", func() (interface{}, error) {
		var gpas []float64
		query := "SELECT score/100/credit AS gpa FROM user_scores"
		err := h.DB.Select(&gpas, query)

		return Gpas{
			Gpas:   gpas,
			GpaAvg: averageFloat64(gpas, 0),
			GpaMax: maxFloat64(gpas, 0),
			GpaMin: minFloat64(gpas, 0),
		}, err
	})

	// 履修している科目一覧取得
	query := "SELECT `courses`.*" +
		" FROM `registrations`" +
		" JOIN `courses` ON `registrations`.`course_id` = `courses`.`id`" +
		" WHERE `user_id` = ?"
	rows, err := h.DB.Queryx(query, userID)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer rows.Close()

	// 科目毎の成績計算処理
	courseResults := make([]CourseResult, 0)
	myGPA := 0.0
	myCredits := 0
	for rows.Next() {
		var course Course
		if err := rows.StructScan(&course); err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}

		chCourse := gpasGroup.DoChan(course.ID, func() (interface{}, error) {
			// この科目を履修している学生のTotalScore一覧を取得
			var totals []int
			query := "SELECT IFNULL(score,0) FROM registrations LEFT JOIN total_scores USING (user_id, course_id) where registrations.course_id = ?"
			err := h.DB.Select(&totals, query, course.ID)
			return Totals{
				Scores:   totals,
				ScoreAvg: averageInt(totals, 0),
				ScoreMax: maxInt(totals, 0),
				ScoreMin: minInt(totals, 0),
			}, err
		})

		// 講義一覧の取得
		classScores := make([]ClassScore, 0)
		query := "SELECT C.id, C.part, C.title, S.score" +
			" FROM `classes` C" +
			" LEFT JOIN submissions S ON C.id = S.class_id AND S.user_id = ?" +
			" WHERE C.`course_id` = ?" +
			" ORDER BY C.`part` DESC"
		classRows, err := h.DB.Queryx(query, userID, course.ID)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}

		// 講義毎の成績計算処理
		var myTotalScore int
		for classRows.Next() {
			cs := ClassScore{}
			if err := classRows.Scan(&cs.ClassID, &cs.Part, &cs.Title, &cs.Score); err != nil {
				c.Logger().Error(err)
				return c.NoContent(http.StatusInternalServerError)
			}
			if v, ok := mapSubmitters.Load(cs.ClassID); ok {
				cs.Submitters = int(*v.(*int64))
			}
			if cs.Score != nil {
				myTotalScore += *cs.Score
			}
			classScores = append(classScores, cs)
		}
		classRows.Close()

		results := <-chCourse
		if err := results.Err; err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		totals := results.Val.(Totals)

		courseResults = append(courseResults, CourseResult{
			Name:             course.Name,
			Code:             course.Code,
			TotalScore:       myTotalScore,
			TotalScoreTScore: tScoreInt(myTotalScore, totals.Scores),
			TotalScoreAvg:    totals.ScoreAvg,
			TotalScoreMax:    totals.ScoreMax,
			TotalScoreMin:    totals.ScoreMin,
			ClassScores:      classScores,
		})

		// 自分のGPA計算
		if course.Status == StatusClosed {
			myGPA += float64(myTotalScore * int(course.Credit))
			myCredits += int(course.Credit)
		}
	}
	if myCredits > 0 {
		myGPA = myGPA / 100 / float64(myCredits)
	}

	gpasResult := <-ch
	if err := gpasResult.Err; err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	gpas := gpasResult.Val.(Gpas)

	res := GetGradeResponse{
		Summary: Summary{
			Credits:   myCredits,
			GPA:       myGPA,
			GpaTScore: tScoreFloat64(myGPA, gpas.Gpas),
			GpaAvg:    gpas.GpaAvg,
			GpaMax:    gpas.GpaMax,
			GpaMin:    gpas.GpaMin,
		},
		CourseResults: courseResults,
	}

	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return json.NewEncoder(c.Response()).Encode(res)
}

// ---------- Courses API ----------

// SearchCourses GET /api/courses 科目検索
func (h *handlers) SearchCourses(c echo.Context) error {
	query := "SELECT `courses`.*, `users`.`name` AS `teacher`" +
		" FROM `courses` JOIN `users` ON `courses`.`teacher_id` = `users`.`id`" +
		" WHERE 1=1"
	var condition string
	var args []interface{}

	// 無効な検索条件はエラーを返さず無視して良い

	if courseType := c.QueryParam("type"); courseType != "" {
		condition += " AND `courses`.`type` = ?"
		args = append(args, courseType)
	}

	if credit, err := strconv.Atoi(c.QueryParam("credit")); err == nil && credit > 0 {
		condition += " AND `courses`.`credit` = ?"
		args = append(args, credit)
	}

	if teacher := c.QueryParam("teacher"); teacher != "" {
		condition += " AND `users`.`name` = ?"
		args = append(args, teacher)
	}

	if period, err := strconv.Atoi(c.QueryParam("period")); err == nil && period > 0 {
		condition += " AND `courses`.`period` = ?"
		args = append(args, period)
	}

	if dayOfWeek := c.QueryParam("day_of_week"); dayOfWeek != "" {
		condition += " AND `courses`.`day_of_week` = ?"
		args = append(args, dayOfWeek)
	}

	if keywords := c.QueryParam("keywords"); keywords != "" {
		arr := strings.Split(keywords, " ")
		var nameCondition string
		for _, keyword := range arr {
			nameCondition += " AND `courses`.`name` LIKE ?"
			args = append(args, "%"+keyword+"%")
		}
		var keywordsCondition string
		for _, keyword := range arr {
			keywordsCondition += " AND `courses`.`keywords` LIKE ?"
			args = append(args, "%"+keyword+"%")
		}
		condition += fmt.Sprintf(" AND ((1=1%s) OR (1=1%s))", nameCondition, keywordsCondition)
	}

	if status := c.QueryParam("status"); status != "" {
		condition += " AND `courses`.`status` = ?"
		args = append(args, status)
	}

	condition += " ORDER BY `courses`.`code`"

	var page int
	if c.QueryParam("page") == "" {
		page = 1
	} else {
		var err error
		page, err = strconv.Atoi(c.QueryParam("page"))
		if err != nil || page <= 0 {
			return c.String(http.StatusBadRequest, "Invalid page.")
		}
	}
	limit := 20
	offset := limit * (page - 1)

	// limitより多く上限を設定し、実際にlimitより多くレコードが取得できた場合は次のページが存在する
	condition += " LIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)

	// 結果が0件の時は空配列を返却
	res := make([]GetCourseDetailResponse, 0)
	if err := h.DB.Select(&res, query+condition, args...); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var links []string
	linkURL, err := url.Parse(c.Request().URL.Path + "?" + c.Request().URL.RawQuery)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	q := linkURL.Query()
	if page > 1 {
		q.Set("page", strconv.Itoa(page-1))
		linkURL.RawQuery = q.Encode()
		links = append(links, fmt.Sprintf("<%v>; rel=\"prev\"", linkURL))
	}
	if len(res) > limit {
		q.Set("page", strconv.Itoa(page+1))
		linkURL.RawQuery = q.Encode()
		links = append(links, fmt.Sprintf("<%v>; rel=\"next\"", linkURL))
	}
	if len(links) > 0 {
		c.Response().Header().Set("Link", strings.Join(links, ","))
	}

	if len(res) == limit+1 {
		res = res[:len(res)-1]
	}

	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return json.NewEncoder(c.Response()).Encode(res)
}

type AddCourseRequest struct {
	Code        string     `json:"code"`
	Type        CourseType `json:"type"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Credit      int        `json:"credit"`
	Period      int        `json:"period"`
	DayOfWeek   DayOfWeek  `json:"day_of_week"`
	Keywords    string     `json:"keywords"`
}

type AddCourseResponse struct {
	ID string `json:"id"`
}

// AddCourse POST /api/courses 新規科目登録
func (h *handlers) AddCourse(c echo.Context) error {
	userID, _, _, err := getUserInfo(c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var req AddCourseRequest
	if err := c.Bind(&req); err != nil {
		return c.String(http.StatusBadRequest, "Invalid format.")
	}

	if req.Type != LiberalArts && req.Type != MajorSubjects {
		return c.String(http.StatusBadRequest, "Invalid course type.")
	}
	if !contains(daysOfWeek, req.DayOfWeek) {
		return c.String(http.StatusBadRequest, "Invalid day of week.")
	}

	courseID := newULID()
	_, err = h.DB.Exec("INSERT INTO `courses` (`id`, `code`, `type`, `name`, `description`, `credit`, `period`, `day_of_week`, `teacher_id`, `keywords`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		courseID, req.Code, req.Type, req.Name, req.Description, req.Credit, req.Period, req.DayOfWeek, userID, req.Keywords)
	if err != nil {
		if mysqlErr, ok := err.(*mysql.MySQLError); ok && mysqlErr.Number == uint16(mysqlErrNumDuplicateEntry) {
			var course Course
			if err := h.DB.Get(&course, "SELECT * FROM `courses` WHERE `code` = ?", req.Code); err != nil {
				c.Logger().Error(err)
				return c.NoContent(http.StatusInternalServerError)
			}
			if req.Type != course.Type || req.Name != course.Name || req.Description != course.Description || req.Credit != int(course.Credit) || req.Period != int(course.Period) || req.DayOfWeek != course.DayOfWeek || req.Keywords != course.Keywords {
				return c.String(http.StatusConflict, "A course with the same code already exists.")
			}
			return c.JSON(http.StatusCreated, AddCourseResponse{ID: course.ID})
		}
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSON(http.StatusCreated, AddCourseResponse{ID: courseID})
}

type GetCourseDetailResponse struct {
	ID          string       `json:"id" db:"id"`
	Code        string       `json:"code" db:"code"`
	Type        string       `json:"type" db:"type"`
	Name        string       `json:"name" db:"name"`
	Description string       `json:"description" db:"description"`
	Credit      uint8        `json:"credit" db:"credit"`
	Period      uint8        `json:"period" db:"period"`
	DayOfWeek   string       `json:"day_of_week" db:"day_of_week"`
	TeacherID   string       `json:"-" db:"teacher_id"`
	Keywords    string       `json:"keywords" db:"keywords"`
	Status      CourseStatus `json:"status" db:"status"`
	Teacher     string       `json:"teacher" db:"teacher"`
}

// GetCourseDetail GET /api/courses/:courseID 科目詳細の取得
func (h *handlers) GetCourseDetail(c echo.Context) error {
	courseID := c.Param("courseID")

	var res GetCourseDetailResponse
	query := "SELECT `courses`.*, `users`.`name` AS `teacher`" +
		" FROM `courses`" +
		" JOIN `users` ON `courses`.`teacher_id` = `users`.`id`" +
		" WHERE `courses`.`id` = ?"
	if err := h.DB.Get(&res, query, courseID); err != nil && err != sql.ErrNoRows {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	} else if err == sql.ErrNoRows {
		return c.String(http.StatusNotFound, "No such course.")
	}

	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return json.NewEncoder(c.Response()).Encode(res)
}

type SetCourseStatusRequest struct {
	Status CourseStatus `json:"status"`
}

// SetCourseStatus PUT /api/courses/:courseID/status 科目のステータスを変更
func (h *handlers) SetCourseStatus(c echo.Context) error {
	courseID := c.Param("courseID")

	var req SetCourseStatusRequest
	if err := c.Bind(&req); err != nil {
		return c.String(http.StatusBadRequest, "Invalid format.")
	}

	res, err := h.DB.Exec("UPDATE `courses` SET `status` = ? WHERE `id` = ?", req.Status, courseID)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	count, err := res.RowsAffected()
	if count == 0 {
		return c.String(http.StatusNotFound, "No such course.")
	}
	if req.Status == StatusClosed {
		if _, err := h.DB.Exec("INSERT INTO `user_scores` (`id`, `score`, `credit`) SELECT R.user_id, IFNULL(SUM(score * credit),0), credit FROM registrations R join courses CO ON R.course_id = CO.id LEFT JOIN classes CL ON CO.id = CL.course_id LEFT JOIN submissions S ON R.user_id = S.user_id AND S.class_id = CL.id WHERE CO.id = ? GROUP BY R.user_id ON DUPLICATE KEY UPDATE score = score + VALUES(`score`), credit = credit + VALUES(`credit`)", courseID); err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	return c.NoContent(http.StatusOK)
}

type ClassWithSubmitted struct {
	ID               string `db:"id"`
	CourseID         string `db:"course_id"`
	Part             uint8  `db:"part"`
	Title            string `db:"title"`
	Description      string `db:"description"`
	SubmissionClosed bool   `db:"submission_closed"`
	Submitted        bool   `db:"submitted"`
}

type GetClassResponse struct {
	ID               string `json:"id"`
	Part             uint8  `json:"part"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	SubmissionClosed bool   `json:"submission_closed"`
	Submitted        bool   `json:"submitted"`
}

// GetClasses GET /api/courses/:courseID/classes 科目に紐づく講義一覧の取得
func (h *handlers) GetClasses(c echo.Context) error {
	userID, _, _, err := getUserInfo(c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	courseID := c.Param("courseID")

	var exists bool
	if err := h.DB.Get(&exists, "SELECT EXISTS(SELECT id FROM `courses` WHERE `id` = ?)", courseID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if !exists {
		return c.String(http.StatusNotFound, "No such course.")
	}

	query := "SELECT `classes`.id, classes.part, classes.title, classes.description, classes.submission_closed, `submissions`.`user_id` IS NOT NULL AS `submitted`" +
		" FROM `classes`" +
		" LEFT JOIN `submissions` ON `classes`.`id` = `submissions`.`class_id` AND `submissions`.`user_id` = ?" +
		" WHERE `classes`.`course_id` = ?" +
		" ORDER BY `classes`.`part`"
	rows, err := h.DB.Queryx(query, userID, courseID)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer rows.Close()

	// 結果が0件の時は空配列を返却
	res := make([]GetClassResponse, 0)
	for rows.Next() {
		var class ClassWithSubmitted
		rows.StructScan(&class)
		res = append(res, GetClassResponse{
			ID:               class.ID,
			Part:             class.Part,
			Title:            class.Title,
			Description:      class.Description,
			SubmissionClosed: class.SubmissionClosed,
			Submitted:        class.Submitted,
		})
	}

	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return json.NewEncoder(c.Response()).Encode(res)
}

type AddClassRequest struct {
	Part        uint8  `json:"part"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type AddClassResponse struct {
	ClassID string `json:"class_id"`
}

// AddClass POST /api/courses/:courseID/classes 新規講義(&課題)追加
func (h *handlers) AddClass(c echo.Context) error {
	courseID := c.Param("courseID")

	var req AddClassRequest
	if err := c.Bind(&req); err != nil {
		return c.String(http.StatusBadRequest, "Invalid format.")
	}

	tx, err := h.DB.Beginx()
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	var course Course
	if err := tx.Get(&course, "SELECT status FROM `courses` WHERE `id` = ? FOR SHARE", courseID); err != nil && err != sql.ErrNoRows {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	} else if err == sql.ErrNoRows {
		return c.String(http.StatusNotFound, "No such course.")
	}
	if course.Status != StatusInProgress {
		return c.String(http.StatusBadRequest, "This course is not in-progress.")
	}

	classID := newULID()
	if _, err := tx.Exec("INSERT INTO `classes` (`id`, `course_id`, `part`, `title`, `description`) VALUES (?, ?, ?, ?, ?)",
		classID, courseID, req.Part, req.Title, req.Description); err != nil {
		_ = tx.Rollback()
		if mysqlErr, ok := err.(*mysql.MySQLError); ok && mysqlErr.Number == uint16(mysqlErrNumDuplicateEntry) {
			var class Class
			if err := h.DB.Get(&class, "SELECT * FROM `classes` WHERE `course_id` = ? AND `part` = ?", courseID, req.Part); err != nil {
				c.Logger().Error(err)
				return c.NoContent(http.StatusInternalServerError)
			}
			if req.Title != class.Title || req.Description != class.Description {
				return c.String(http.StatusConflict, "A class with the same part already exists.")
			}
			return c.JSON(http.StatusCreated, AddClassResponse{ClassID: class.ID})
		}
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if err := os.Mkdir(filepath.Join(AssignmentsDirectory, classID), 0755); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if err := tx.Commit(); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSON(http.StatusCreated, AddClassResponse{ClassID: classID})
}

// SubmitAssignment POST /api/courses/:courseID/classes/:classID/assignments 課題の提出
func (h *handlers) SubmitAssignment(c echo.Context) error {
	userID, _, _, err := getUserInfo(c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	courseID := c.Param("courseID")
	classID := c.Param("classID")

	tx, err := h.DB.Beginx()
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	var status CourseStatus
	if err := tx.Get(&status, "SELECT `status` FROM `courses` WHERE `id` = ? FOR SHARE", courseID); err != nil && err != sql.ErrNoRows {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	} else if err == sql.ErrNoRows {
		return c.String(http.StatusNotFound, "No such course.")
	}
	if status != StatusInProgress {
		return c.String(http.StatusBadRequest, "This course is not in progress.")
	}

	var code sql.NullString
	if err := tx.Get(&code, "SELECT code FROM users JOIN `registrations` ON users.id = registrations.user_id WHERE `user_id` = ? AND `course_id` = ?", userID, courseID); err != nil && err != sql.ErrNoRows {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if !code.Valid {
		return c.String(http.StatusBadRequest, "You have not taken this course.")
	}

	var submissionClosed bool
	if err := tx.Get(&submissionClosed, "SELECT `submission_closed` FROM `classes` WHERE `id` = ? FOR SHARE", classID); err != nil && err != sql.ErrNoRows {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	} else if err == sql.ErrNoRows {
		return c.String(http.StatusNotFound, "No such class.")
	}
	if submissionClosed {
		return c.String(http.StatusBadRequest, "Submission has been closed for this class.")
	}

	file, header, err := c.Request().FormFile("file")
	if err != nil {
		return c.String(http.StatusBadRequest, "Invalid file.")
	}
	defer file.Close()

	if _, err := tx.Exec("INSERT INTO `submissions` (`user_id`, `class_id`, `file_name`) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE `file_name` = VALUES(`file_name`)", userID, classID, header.Filename); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	v, _ := mapSubmitters.LoadOrStore(classID, new(int64))
	atomic.AddInt64(v.(*int64), 1)

	dst := filepath.Join(AssignmentsDirectory, classID, code.String+"-"+header.Filename)
	pdf, err := os.Create(dst)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer pdf.Close()

	if _, err := io.Copy(pdf, file); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if err := tx.Commit(); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusNoContent)
}

type Score struct {
	UserCode string `json:"user_code"`
	Score    int    `json:"score"`
}

// RegisterScores PUT /api/courses/:courseID/classes/:classID/assignments/scores 採点結果登録
func (h *handlers) RegisterScores(c echo.Context) error {
	classID := c.Param("classID")

	tx, err := h.DB.Beginx()
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	var submissionClosed bool
	if err := tx.Get(&submissionClosed, "SELECT `submission_closed` FROM `classes` WHERE `id` = ? FOR SHARE", classID); err != nil && err != sql.ErrNoRows {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	} else if err == sql.ErrNoRows {
		return c.String(http.StatusNotFound, "No such class.")
	}

	if !submissionClosed {
		return c.String(http.StatusBadRequest, "This assignment is not closed yet.")
	}

	var req []Score
	if err := c.Bind(&req); err != nil {
		return c.String(http.StatusBadRequest, "Invalid format.")
	}

	if len(req) > 0 {
		scores := make([]string, 0, len(req))
		codes := make([]string, 0, len(req))
		for _, score := range req {
			scores = append(scores, strconv.Itoa(score.Score))
			codes = append(codes, "'"+score.UserCode+"'")
		}
		query := fmt.Sprintf("UPDATE `submissions` JOIN `users` ON `users`.`id` = `submissions`.`user_id` SET `score` = ELT(FIELD(code,%s),%s) WHERE `users`.`code` IN (%s) AND `class_id` = ?", strings.Join(codes, ","), strings.Join(scores, ","), strings.Join(codes, ","))
		if _, err := tx.Exec(query, classID); err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		query = fmt.Sprintf("INSERT INTO total_scores (user_id, course_id, score) SELECT registrations.user_id, registrations.course_id, ELT(FIELD(code,%s),%s) FROM users JOIN registrations ON users.id = registrations.user_id JOIN classes ON registrations.course_id = classes.course_id WHERE users.code IN (%s) AND classes.id = ? ON DUPLICATE KEY UPDATE score = score + VALUES(score)", strings.Join(codes, ","), strings.Join(scores, ","), strings.Join(codes, ","))
		if _, err := tx.Exec(query, classID); err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	if err := tx.Commit(); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusNoContent)
}

type Submission struct {
	UserID   string `db:"user_id"`
	UserCode string `db:"user_code"`
	FileName string `db:"file_name"`
}

// DownloadSubmittedAssignments GET /api/courses/:courseID/classes/:classID/assignments/export 提出済みの課題ファイルをzip形式で一括ダウンロード
func (h *handlers) DownloadSubmittedAssignments(c echo.Context) error {
	classID := c.Param("classID")

	tx, err := h.DB.Beginx()
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	var classCount int
	if err := tx.Get(&classCount, "SELECT COUNT(id) FROM `classes` WHERE `id` = ? FOR UPDATE", classID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if classCount == 0 {
		return c.String(http.StatusNotFound, "No such class.")
	}

	if _, err := tx.Exec("UPDATE `classes` SET `submission_closed` = true WHERE `id` = ?", classID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if err := tx.Commit(); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	c.Response().Header().Set(echo.HeaderContentType, "application/zip")
	c.Response().WriteHeader(http.StatusOK)
	zw := zip.NewWriter(c.Response())
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.NoCompression)
	})
	defer zw.Close()

	dir := filepath.Join(AssignmentsDirectory, classID)
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		pw, err := zw.Create(d.Name())
		if err != nil {
			return err
		}
		pdf, err := os.Open(path)
		if err != nil {
			return err
		}
		defer pdf.Close()
		if _, err := io.Copy(pw, pdf); err != nil {
			return err
		}
		return nil
	})
}

// ---------- Announcement API ----------

type AnnouncementWithoutDetail struct {
	ID         string `json:"id" db:"id"`
	CourseID   string `json:"course_id" db:"course_id"`
	CourseName string `json:"course_name" db:"course_name"`
	Title      string `json:"title" db:"title"`
	Unread     bool   `json:"unread" db:"unread"`
}

type GetAnnouncementsResponse struct {
	UnreadCount   int                         `json:"unread_count"`
	Announcements []AnnouncementWithoutDetail `json:"announcements"`
}

// GetAnnouncementList GET /api/announcements お知らせ一覧取得
func (h *handlers) GetAnnouncementList(c echo.Context) error {
	userID, _, _, err := getUserInfo(c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	announcements := make([]AnnouncementWithoutDetail, 0, 21)
	var args []interface{}
	query := "SELECT `announcements`.`id`, `courses`.`id` AS `course_id`, `courses`.`name` AS `course_name`, `announcements`.`title`, `unread_announcements`.`announcement_id` IS NOT NULL AS `unread`" +
		" FROM `announcements`" +
		" JOIN `courses` ON `announcements`.`course_id` = `courses`.`id`" +
		" JOIN `registrations` ON `courses`.`id` = `registrations`.`course_id`" +
		" LEFT JOIN `unread_announcements` ON `announcements`.`id` = `unread_announcements`.`announcement_id` AND `unread_announcements`.`user_id` = ?" +
		" WHERE 1=1"
	args = append(args, userID)

	if courseID := c.QueryParam("course_id"); courseID != "" {
		query += " AND `announcements`.`course_id` = ?"
		args = append(args, courseID)
	}

	query += " AND `registrations`.`user_id` = ?" +
		" ORDER BY `announcements`.`id` DESC" +
		" LIMIT ? OFFSET ?"
	args = append(args, userID)

	var page int
	if c.QueryParam("page") == "" {
		page = 1
	} else {
		page, err = strconv.Atoi(c.QueryParam("page"))
		if err != nil || page <= 0 {
			return c.String(http.StatusBadRequest, "Invalid page.")
		}
	}
	limit := 20
	offset := limit * (page - 1)
	// limitより多く上限を設定し、実際にlimitより多くレコードが取得できた場合は次のページが存在する
	args = append(args, limit+1, offset)

	ch := make(chan int)
	go func() {
		defer close(ch)
		var unreadCount int
		if err := h.DB.Get(&unreadCount, "SELECT COUNT(user_id) FROM `unread_announcements` WHERE `user_id` = ?", userID); err != nil {
			c.Logger().Error(err)
			return
			// return c.NoContent(http.StatusInternalServerError)
		}
		ch <- unreadCount
	}()

	if err := h.DB.Select(&announcements, query, args...); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var links []string
	linkURL, err := url.Parse(c.Request().URL.Path + "?" + c.Request().URL.RawQuery)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	q := linkURL.Query()
	if page > 1 {
		q.Set("page", strconv.Itoa(page-1))
		linkURL.RawQuery = q.Encode()
		links = append(links, fmt.Sprintf("<%v>; rel=\"prev\"", linkURL))
	}
	if len(announcements) > limit {
		q.Set("page", strconv.Itoa(page+1))
		linkURL.RawQuery = q.Encode()
		links = append(links, fmt.Sprintf("<%v>; rel=\"next\"", linkURL))
	}
	if len(links) > 0 {
		c.Response().Header().Set("Link", strings.Join(links, ","))
	}

	if len(announcements) == limit+1 {
		announcements = announcements[:len(announcements)-1]
	}

	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return json.NewEncoder(c.Response()).Encode(GetAnnouncementsResponse{
		UnreadCount:   <-ch,
		Announcements: announcements,
	})
}

type Announcement struct {
	ID       string `db:"id"`
	CourseID string `db:"course_id"`
	Title    string `db:"title"`
	Message  string `db:"message"`
}

type AddAnnouncementRequest struct {
	ID       string `json:"id"`
	CourseID string `json:"course_id"`
	Title    string `json:"title"`
	Message  string `json:"message"`
}

// AddAnnouncement POST /api/announcements 新規お知らせ追加
func (h *handlers) AddAnnouncement(c echo.Context) error {
	var req AddAnnouncementRequest
	if err := c.Bind(&req); err != nil {
		return c.String(http.StatusBadRequest, "Invalid format.")
	}

	tx, err := h.DB.Beginx()
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	var count int
	if err := tx.Get(&count, "SELECT COUNT(id) FROM `courses` WHERE `id` = ?", req.CourseID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if count == 0 {
		return c.String(http.StatusNotFound, "No such course.")
	}

	if _, err := tx.Exec("INSERT INTO `announcements` (`id`, `course_id`, `title`, `message`) VALUES (?, ?, ?, ?)",
		req.ID, req.CourseID, req.Title, req.Message); err != nil {
		_ = tx.Rollback()
		if mysqlErr, ok := err.(*mysql.MySQLError); ok && mysqlErr.Number == uint16(mysqlErrNumDuplicateEntry) {
			var announcement Announcement
			if err := h.DB.Get(&announcement, "SELECT * FROM `announcements` WHERE `id` = ?", req.ID); err != nil {
				c.Logger().Error(err)
				return c.NoContent(http.StatusInternalServerError)
			}
			if announcement.CourseID != req.CourseID || announcement.Title != req.Title || announcement.Message != req.Message {
				return c.String(http.StatusConflict, "An announcement with the same id already exists.")
			}
			return c.NoContent(http.StatusCreated)
		}
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	query := "INSERT INTO `unread_announcements` (announcement_id, user_id) SELECT ?, user_id FROM `registrations` WHERE `course_id` = ?"
	if _, err := tx.Exec(query, req.ID, req.CourseID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if err := tx.Commit(); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusCreated)
}

type AnnouncementDetail struct {
	ID         string `json:"id" db:"id"`
	CourseID   string `json:"course_id" db:"course_id"`
	CourseName string `json:"course_name" db:"course_name"`
	Title      string `json:"title" db:"title"`
	Message    string `json:"message" db:"message"`
	Unread     bool   `json:"unread" db:"unread"`
}

// GetAnnouncementDetail GET /api/announcements/:announcementID お知らせ詳細取得
func (h *handlers) GetAnnouncementDetail(c echo.Context) error {
	userID, _, _, err := getUserInfo(c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	announcementID := c.Param("announcementID")

	var announcement AnnouncementDetail
	query := "SELECT `announcements`.`id`, `courses`.`id` AS `course_id`, `courses`.`name` AS `course_name`, `announcements`.`title`, `announcements`.`message`, `unread_announcements`.`announcement_id` IS NOT NULL AS `unread`" +
		" FROM `announcements`" +
		" JOIN `courses` ON `courses`.`id` = `announcements`.`course_id`" +
		" LEFT JOIN `unread_announcements` ON `unread_announcements`.`announcement_id` = `announcements`.`id` AND `unread_announcements`.`user_id` = ?" +
		" WHERE `announcements`.`id` = ?"
	if err := h.DB.Get(&announcement, query, userID, announcementID); err != nil && err != sql.ErrNoRows {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	} else if err == sql.ErrNoRows {
		return c.String(http.StatusNotFound, "No such announcement.")
	}

	var registrationExists bool
	if err := h.DB.Get(&registrationExists, "SELECT EXISTS(SELECT 1 FROM `registrations` WHERE `course_id` = ? AND `user_id` = ?)", announcement.CourseID, userID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if !registrationExists {
		return c.String(http.StatusNotFound, "No such announcement.")
	}

	if _, err := h.DB.Exec("DELETE FROM `unread_announcements` WHERE `announcement_id` = ? AND `user_id` = ?", announcementID, userID); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return json.NewEncoder(c.Response()).Encode(announcement)
}
