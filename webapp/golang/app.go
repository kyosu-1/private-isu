package main

import (
	crand "crypto/rand"
	"database/sql"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"crypto/sha512"
	"encoding/hex"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/kaz/pprotein/integration/standalone"
)

var (
	db                *sqlx.DB
	store             *gsm.MemcacheStore
	compiledTemplates = make(map[string]*template.Template)
)

var (
	fmap = template.FuncMap{
		"imageURL": imageURL,
	}
	indexTemplate = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("index.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
	loginTemplate = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html"),
	))
	registerTemplate = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html"),
	))
	postsTemplate = template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
	postIdTemplate = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html"),
	))
	accountNameTemplate = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
	adminBannedTemplate = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html"),
	))
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
	ImageDir      = "../public/image/"
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient := memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func dbInitialize() {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.Exec(sql)
	}
}

func tryLogin(accountName, password string) *User {
	u := User{}
	err := db.Get(&u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

func escapeshellarg(arg string) string {
	return "'" + strings.Replace(arg, "'", "'\\''", -1) + "'"
}

func digest(src string) string {
	hash := sha512.Sum512([]byte(src))
	return hex.EncodeToString(hash[:])
}

func calculateSalt(accountName string) string {
	return digest(accountName)
}

func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	u := User{}

	err := db.Get(&u, "SELECT * FROM `users` WHERE `id` = ?", uid)
	if err != nil {
		return User{}
	}

	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

func makePosts(results []Post, csrfToken string, allComments bool) ([]Post, error) {
	var posts []Post

	for _, p := range results {
		err := db.Get(&p.CommentCount, "SELECT COUNT(*) AS `count` FROM `comments` WHERE `post_id` = ?", p.ID)
		if err != nil {
			return nil, err
		}

		query := "SELECT * FROM `comments` WHERE `post_id` = ? ORDER BY `created_at` DESC"
		if !allComments {
			query += " LIMIT 3"
		}
		var comments []Comment
		err = db.Select(&comments, query, p.ID)
		if err != nil {
			return nil, err
		}

		for i := 0; i < len(comments); i++ {
			err := db.Get(&comments[i].User, "SELECT * FROM `users` WHERE `id` = ?", comments[i].UserID)
			if err != nil {
				return nil, err
			}
		}

		// reverse
		for i, j := 0, len(comments)-1; i < j; i, j = i+1, j-1 {
			comments[i], comments[j] = comments[j], comments[i]
		}

		p.Comments = comments

		err = db.Get(&p.User, "SELECT * FROM `users` WHERE `id` = ?", p.UserID)
		if err != nil {
			return nil, err
		}

		p.CSRFToken = csrfToken

		if p.User.DelFlg == 0 {
			posts = append(posts, p)
		}
		if len(posts) >= postsPerPage {
			break
		}
	}

	return posts, nil
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func handleError(w http.ResponseWriter, err error, msg string) bool {
	if err != nil {
		log.Printf("%s: %v", msg, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return true
	}
	return false
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	dbInitialize()
	go func() {
		if _, err := http.Get("http://localhost:9000/api/group/collect"); err != nil {
			log.Printf("failed to communicate with pprotein: %v", err)
		}
	}()
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	loginTemplate.Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	registerTemplate.Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	db.Get(&exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.Exec(query, accountName, calculatePasshash(accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLatestPosts(limit int) ([]Post, error) {
	var posts []Post
	query := `
		SELECT 
			p.id, p.user_id, p.body, p.mime, p.created_at,
			u.id AS "user.id", u.account_name AS "user.account_name", 
			u.del_flg AS "user.del_flg", u.passhash AS "user.passhash", 
			u.authority AS "user.authority", u.created_at AS "user.created_at"
		FROM posts p USE INDEX (idx_created_at)
		JOIN users u ON p.user_id = u.id
		WHERE u.del_flg = 0
		ORDER BY p.created_at DESC
		LIMIT ?`
	err := db.Select(&posts, query, limit)
	return posts, err
}

func getCommentsForPosts(postIDs []int, limitPerPost int) ([]Comment, error) {
	if len(postIDs) == 0 {
		return nil, nil
	}

	query := `
		SELECT id, post_id, user_id, comment, created_at
		FROM (
			SELECT 
				c.id, c.post_id, c.user_id, c.comment, c.created_at,
				ROW_NUMBER() OVER (PARTITION BY c.post_id ORDER BY c.created_at DESC) AS rn
			FROM comments c
			WHERE c.post_id IN (?) 
		) sub
		WHERE sub.rn <= ?
		ORDER BY sub.post_id, sub.created_at DESC`

	query, args, err := sqlx.In(query, postIDs, limitPerPost)
	if err != nil {
		return nil, err
	}
	query = db.Rebind(query)

	var comments []Comment
	err = db.Select(&comments, query, args...)
	return comments, err
}

func getUsersByIDs(userIDs []int) ([]User, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}

	query, args, err := sqlx.In("SELECT id, account_name FROM users WHERE id IN (?)", userIDs)
	if err != nil {
		return nil, err
	}
	query = db.Rebind(query)

	var users []User
	err = db.Select(&users, query, args...)
	return users, err
}

func fetchCommentsAndUsers(postIDs []int, limitPerPost int) (map[int][]Comment, error) {
	comments, err := getCommentsForPosts(postIDs, limitPerPost)
	if err != nil {
		return nil, err
	}

	commentUserIDsMap := make(map[int]struct{})
	for _, comment := range comments {
		commentUserIDsMap[comment.UserID] = struct{}{}
	}
	commentUserIDs := make([]int, 0, len(commentUserIDsMap))
	for uid := range commentUserIDsMap {
		commentUserIDs = append(commentUserIDs, uid)
	}

	commentUsers, err := getUsersByIDs(commentUserIDs)
	if err != nil {
		return nil, err
	}

	commentUserMap := make(map[int]User)
	for _, user := range commentUsers {
		commentUserMap[user.ID] = user
	}

	for i := range comments {
		if user, exists := commentUserMap[comments[i].UserID]; exists {
			comments[i].User = user
		}
	}

	postCommentsMap := make(map[int][]Comment)
	for _, comment := range comments {
		postCommentsMap[comment.PostID] = append(postCommentsMap[comment.PostID], comment)
	}

	return postCommentsMap, nil
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	posts, err := getLatestPosts(postsPerPage)
	if handleError(w, err, "Failed to get latest posts") {
		return
	}

	postIDs := make([]int, len(posts))
	for i, post := range posts {
		postIDs[i] = post.ID
	}

	postCommentsMap, err := fetchCommentsAndUsers(postIDs, 3)
	if handleError(w, err, "Failed to fetch comments and users") {
		return
	}

	csrfToken := getCSRFToken(r)
	for i := range posts {
		posts[i].Comments = postCommentsMap[posts[i].ID]
		posts[i].CommentCount = len(postCommentsMap[posts[i].ID])
		posts[i].CSRFToken = csrfToken
	}

	indexTemplate.Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, csrfToken, getFlash(w, r, "notice")})
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	accountName := r.PathValue("accountName")
	user := User{}

	err := db.Get(&user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	posts := []Post{}
	err = db.Select(&posts, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT ?", user.ID, postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	postIDs := make([]int, len(posts))
	for i, post := range posts {
		postIDs[i] = post.ID
	}

	postCommentsMap, err := fetchCommentsAndUsers(postIDs, 3)
	if handleError(w, err, "Failed to fetch comments and users") {
		return
	}

	csrfToken := getCSRFToken(r)
	for i := range posts {
		posts[i].Comments = postCommentsMap[posts[i].ID]
		posts[i].CommentCount = len(postCommentsMap[posts[i].ID])
		posts[i].CSRFToken = csrfToken
	}

	commentCount := 0
	err = db.Get(&commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	postIDs = []int{}
	err = db.Select(&postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}
	postCount := len(postIDs)

	commentedCount := 0
	if postCount > 0 {
		s := []string{}
		for range postIDs {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		args := make([]interface{}, len(postIDs))
		for i, v := range postIDs {
			args[i] = v
		}

		err = db.Get(&commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
		if err != nil {
			log.Print(err)
			return
		}
	}

	me := getSessionUser(r)

	accountNameTemplate.Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	err = db.Select(&results, `
		SELECT 
			p.id, p.user_id, p.body, p.mime, p.created_at,
			u.id AS "user.id", u.account_name AS "user.account_name", 
			u.del_flg AS "user.del_flg", u.passhash AS "user.passhash", 
			u.authority AS "user.authority", u.created_at AS "user.created_at"
		FROM posts p USE INDEX (idx_created_at)
		JOIN users u ON p.user_id = u.id
		WHERE p.created_at <= ? AND u.del_flg = 0
		ORDER BY p.created_at DESC
		LIMIT ?`, t.Format(ISO8601Format), postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	postIDs := make([]int, len(results))
	for i, post := range results {
		postIDs[i] = post.ID
	}

	comments, err := getCommentsForPosts(postIDs, 3)
	if handleError(w, err, "Failed to get comments for posts") {
		return
	}

	commentUserIDsMap := make(map[int]struct{})
	for _, comment := range comments {
		commentUserIDsMap[comment.UserID] = struct{}{}
	}
	commentUserIDs := make([]int, 0, len(commentUserIDsMap))
	for uid := range commentUserIDsMap {
		commentUserIDs = append(commentUserIDs, uid)
	}

	commentUsers, err := getUsersByIDs(commentUserIDs)
	if handleError(w, err, "Failed to get users for comments") {
		return
	}

	commentUserMap := make(map[int]User)
	for _, user := range commentUsers {
		commentUserMap[user.ID] = user
	}

	for i := range comments {
		if user, exists := commentUserMap[comments[i].UserID]; exists {
			comments[i].User = user
		}
	}

	postCommentsMap := make(map[int][]Comment)
	for _, comment := range comments {
		postCommentsMap[comment.PostID] = append(postCommentsMap[comment.PostID], comment)
	}
	for i := range results {
		results[i].Comments = postCommentsMap[results[i].ID]
		results[i].CommentCount = len(postCommentsMap[results[i].ID])
	}

	if len(results) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	postsTemplate.Execute(w, results)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	post, err := makePost(pid, getCSRFToken(r))
	if err != nil {
		log.Print(err)
		return
	}

	if post == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	me := getSessionUser(r)

	postIdTemplate.Execute(w, struct {
		Post Post
		Me   User
	}{*post, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	ext := ""
	if file != nil {
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
			ext = "jpg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
			ext = "png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
			ext = "gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, err := db.Exec(
		query,
		me.ID,
		mime,
		"", // データベースには画像データは保存しない
		r.FormValue("body"),
	)
	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	filepath := filepath.Join(ImageDir, fmt.Sprintf("%d.%s", pid, ext))
	err = os.WriteFile(filepath, filedata, 0644)
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getImage(w http.ResponseWriter, r *http.Request) {
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	post := Post{}
	err = db.Get(&post, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	ext := r.PathValue("ext")

	if ext == "jpg" && post.Mime == "image/jpeg" ||
		ext == "png" && post.Mime == "image/png" ||
		ext == "gif" && post.Mime == "image/gif" {
		w.Header().Set("Content-Type", post.Mime)
		_, err := w.Write(post.Imgdata)
		if err != nil {
			log.Print(err)
			return
		}
		return
	}

	filepath := filepath.Join(ImageDir, fmt.Sprintf("%d.%s", pid, ext))
	err = os.WriteFile(filepath, post.Imgdata, 0644)
	if err != nil {
		log.Print(err)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func postComment(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_idは整数のみです")
		return
	}

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = db.Exec(query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.Select(&users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	adminBannedTemplate.Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		db.Exec(query, 1, id)
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func makePost(postID int, csrfToken string) (*Post, error) {
	var post Post

	query := `
		SELECT 
			p.id, p.user_id, p.body, p.mime, p.created_at,
			u.id AS "user.id", u.account_name AS "user.account_name", 
			u.del_flg AS "user.del_flg", u.passhash AS "user.passhash", 
			u.authority AS "user.authority", u.created_at AS "user.created_at"
		FROM posts p
		JOIN users u ON p.user_id = u.id
		WHERE p.id = ? AND u.del_flg = 0`
	err := db.Get(&post, query, postID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	commentsQuery := `
		SELECT 
			c.id, c.post_id, c.user_id, c.comment, c.created_at,
			u.id AS "user.id", u.account_name AS "user.account_name"
		FROM comments c
		JOIN users u ON c.user_id = u.id
		WHERE c.post_id = ?
		ORDER BY c.created_at DESC`
	var comments []Comment
	err = db.Select(&comments, commentsQuery, postID)
	if err != nil {
		return nil, err
	}

	post.Comments = comments
	post.CommentCount = len(comments)
	post.CSRFToken = csrfToken

	return &post, nil
}

func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		user,
		password,
		host,
		port,
		dbname,
	)

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	r := chi.NewRouter()

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Get("/image/{id}.{ext}", getImage)
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[a-zA-Z]+}`, getAccountName)
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		http.FileServer(http.Dir("../public")).ServeHTTP(w, r)
	})

	go func() {
		standalone.Integrate(":6060")
	}()

	log.Fatal(http.ListenAndServe(":8080", r))
}
