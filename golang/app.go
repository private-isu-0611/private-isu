package main

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
)

var (
	db             *sqlx.DB
	store          *gsm.MemcacheStore
	memcacheClient *memcache.Client
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
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
	memcacheClient = memcache.New(memdAddr)
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

// 今回のGo実装では言語側のエスケープの仕組みが使えないのでOSコマンドインジェクション対策できない
// 取り急ぎPHPのescapeshellarg関数を参考に自前で実装
// cf: http://jp2.php.net/manual/ja/function.escapeshellarg.php
func escapeshellarg(arg string) string {
	return "'" + strings.Replace(arg, "'", "'\\''", -1) + "'"
}

func digest(src string) string {
	// opensslのバージョンによっては (stdin)= というのがつくので取る
	out, err := exec.Command("/bin/bash", "-c", `printf "%s" `+escapeshellarg(src)+` | openssl dgst -sha512 | sed 's/^.*= //'`).Output()
	if err != nil {
		log.Print(err)
		return ""
	}

	return strings.TrimSuffix(string(out), "\n")
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

	// キャッシュキーを作成
	cacheKey := fmt.Sprintf("user:%d", uid)
	
	// キャッシュから取得を試みる
	item, err := memcacheClient.Get(cacheKey)
	if err == nil {
		// キャッシュヒット
		u := User{}
		err = json.Unmarshal(item.Value, &u)
		if err == nil {
			return u
		}
	}

	// キャッシュミスまたはデシリアライズ失敗の場合はDBから取得
	u := User{}
	err = db.Get(&u, "SELECT * FROM `users` WHERE `id` = ?", uid)
	if err != nil {
		return User{}
	}

	// キャッシュに保存（有効期限: 300秒）
	data, err := json.Marshal(u)
	if err == nil {
		memcacheClient.Set(&memcache.Item{
			Key:        cacheKey,
			Value:      data,
			Expiration: 300, // 5分
		})
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
	if len(results) == 0 {
		return posts, nil
	}

	// 投稿IDとユーザーIDを収集
	postIDs := make([]int, len(results))
	userIDSet := make(map[int]struct{})
	for i, p := range results {
		postIDs[i] = p.ID
		userIDSet[p.UserID] = struct{}{}
	}

	// 1. 各投稿のコメント数を一括取得
	type countRow struct {
		PostID int `db:"post_id"`
		Count  int `db:"count"`
	}
	var counts []countRow
	countQuery, args, _ := sqlx.In(
		"SELECT post_id, COUNT(*) AS count FROM comments WHERE post_id IN (?) GROUP BY post_id", postIDs,
	)
	countQuery = db.Rebind(countQuery)
	if err := db.Select(&counts, countQuery, args...); err != nil {
		return nil, err
	}
	commentCountMap := make(map[int]int)
	for _, row := range counts {
		commentCountMap[row.PostID] = row.Count
	}

	// 2. コメント本体を一括取得
	var allCommentsList []Comment
	commentQuery := "SELECT * FROM comments WHERE post_id IN (?) ORDER BY created_at DESC"
	commentQuery, args, _ = sqlx.In(commentQuery, postIDs)
	commentQuery = db.Rebind(commentQuery)
	if err := db.Select(&allCommentsList, commentQuery, args...); err != nil {
		return nil, err
	}
	commentsMap := make(map[int][]Comment)
	for _, c := range allCommentsList {
		commentsMap[c.PostID] = append(commentsMap[c.PostID], c)
		userIDSet[c.UserID] = struct{}{}
	}

	// 3. 関連するユーザー情報を取得（キャッシュ活用）
	userIDs := make([]int, 0, len(userIDSet))
	for uid := range userIDSet {
		userIDs = append(userIDs, uid)
	}
	userMap := make(map[int]User)
	
	// まずキャッシュから取得を試みる
	uncachedUserIDs := []int{}
	for _, uid := range userIDs {
		cacheKey := fmt.Sprintf("user:%d", uid)
		item, err := memcacheClient.Get(cacheKey)
		if err == nil {
			// キャッシュヒット
			var u User
			err = json.Unmarshal(item.Value, &u)
			if err == nil {
				userMap[uid] = u
				continue
			}
		}
		// キャッシュミスの場合はリストに追加
		uncachedUserIDs = append(uncachedUserIDs, uid)
	}
	
	// キャッシュにないユーザー情報をDBから一括取得
	if len(uncachedUserIDs) > 0 {
		var users []User
		userQuery, args, _ := sqlx.In("SELECT * FROM users WHERE id IN (?)", uncachedUserIDs)
		userQuery = db.Rebind(userQuery)
		if err := db.Select(&users, userQuery, args...); err != nil {
			return nil, err
		}
		
		// 取得したユーザー情報をキャッシュに保存
		for _, u := range users {
			userMap[u.ID] = u
			
			// キャッシュに保存
			cacheKey := fmt.Sprintf("user:%d", u.ID)
			data, err := json.Marshal(u)
			if err == nil {
				memcacheClient.Set(&memcache.Item{
					Key:        cacheKey,
					Value:      data,
					Expiration: 300, // 5分
				})
			}
		}
	}

	// 4. 投稿データを構築
	for _, p := range results {
		p.CommentCount = commentCountMap[p.ID]

		comments := commentsMap[p.ID]
		if !allComments && len(comments) > 3 {
			comments = comments[:3]
		}
		for i := range comments {
			comments[i].User = userMap[comments[i].UserID]
		}
		// reverse
		for i, j := 0, len(comments)-1; i < j; i, j = i+1, j-1 {
			comments[i], comments[j] = comments[j], comments[i]
		}
		p.Comments = comments

		p.User = userMap[p.UserID]
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
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html")),
	).Execute(w, struct {
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

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html")),
	).Execute(w, struct {
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
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
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

	// 新規登録時はユーザーキャッシュは作成しない（次回取得時に作成される）

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	// キャッシュキーを作成
	cacheKey := "index_posts"

	// キャッシュから取得を試みる
	item, err := memcacheClient.Get(cacheKey)
	var posts []Post

	if err == nil {
		// キャッシュヒット
		err = json.Unmarshal(item.Value, &posts)
		if err != nil {
			log.Print("Failed to unmarshal cache:", err)
			// キャッシュのデシリアライズに失敗した場合はDBから取得
			posts = nil
		}
	}
	
	if err != nil || posts == nil {
		// キャッシュミスまたはデシリアライズ失敗の場合はDBから取得
		results := []Post{}

		err := db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` ORDER BY `created_at` DESC LIMIT 40")
		if err != nil {
			log.Print(err)
			return
		}

		posts, err = makePosts(results, getCSRFToken(r), false)
		if err != nil {
			log.Print(err)
			return
		}

		// キャッシュに保存（有効期限: 60秒）
		if len(posts) > 0 {
			data, err := json.Marshal(posts)
			if err == nil {
				memcacheClient.Set(&memcache.Item{
					Key:        cacheKey,
					Value:      data,
					Expiration: 60, // 60秒
				})
			}
		}
	}

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("index.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	accountName := r.PathValue("accountName")

	// キャッシュキーを作成
	cacheKey := fmt.Sprintf("account:%s", accountName)

	// キャッシュから取得を試みる
	type accountPageData struct {
		User           User   `json:"user"`
		Posts          []Post `json:"posts"`
		CommentCount   int    `json:"comment_count"`
		PostCount      int    `json:"post_count"`
		CommentedCount int    `json:"commented_count"`
	}

	item, err := memcacheClient.Get(cacheKey)
	var data accountPageData

	if err == nil {
		// キャッシュヒット
		err = json.Unmarshal(item.Value, &data)
		if err != nil {
			log.Print("Failed to unmarshal cache:", err)
			data = accountPageData{}
		}
	}

	if err != nil || data.User.ID == 0 {
		// キャッシュミスまたはデシリアライズ失敗の場合はDBから取得
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

		results := []Post{}
		err = db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT 40", user.ID)
		if err != nil {
			log.Print(err)
			return
		}

		posts, err := makePosts(results, getCSRFToken(r), false)
		if err != nil {
			log.Print(err)
			return
		}

		commentCount := 0
		err = db.Get(&commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
		if err != nil {
			log.Print(err)
			return
		}

		postIDs := []int{}
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

			// convert []int -> []interface{}
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

		data = accountPageData{
			User:           user,
			Posts:          posts,
			CommentCount:   commentCount,
			PostCount:      postCount,
			CommentedCount: commentedCount,
		}

		// キャッシュに保存（有効期限: 60秒）
		cacheData, err := json.Marshal(data)
		if err == nil {
			memcacheClient.Set(&memcache.Item{
				Key:        cacheKey,
				Value:      cacheData,
				Expiration: 60, // 60秒
			})
		}
	}

	me := getSessionUser(r)

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{data.Posts, data.User, data.PostCount, data.CommentCount, data.CommentedCount, me})
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
	err = db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `created_at` <= ? ORDER BY `created_at` DESC LIMIT 40", t.Format(ISO8601Format))
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	)).Execute(w, posts)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	err = db.Select(&results, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
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

	mime, ext := "", ""
	if file != nil {
		// 投稿のContent-Typeからファイルのタイプを決定する
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

	// filedata, err := io.ReadAll(file)
	// if err != nil {
	// 	log.Print(err)
	// 	return
	// }

	if header.Size > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	emptyImage := []byte{}
	result, err := db.Exec(
		query,
		me.ID,
		mime,
		emptyImage, // 静的ファイル配信のためNULLを設定
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

	// 画像を静的ファイルとして保存
	saveStaticFile(int(pid), ext, file)

	// キャッシュを無効化
	memcacheClient.Delete("index_posts")
	// 投稿したユーザーのアカウントページキャッシュも無効化
	cacheKey := fmt.Sprintf("account:%s", me.AccountName)
	memcacheClient.Delete(cacheKey)

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func saveStaticFile(pid int, ext string, file multipart.File) {
	os.MkdirAll("../public/image", 0755)
	filePath := fmt.Sprintf("../public/image/%d.%s", pid, ext)

	// ファイルを作成
	dst, err := os.Create(filePath)
	if err != nil {
		log.Print(err)
		return
	}
	defer dst.Close()

	// ストリーミングコピー（メモリに全体を読み込まない）
	_, err = io.Copy(dst, file)
	if err != nil {
		log.Print(err)
		os.Remove(filePath) // エラー時はファイル削除
		return
	}

	// ファイル権限設定
	err = os.Chmod(filePath, 0644)
	if err != nil {
		log.Print(err)
		return
	}
}

func getImage(w http.ResponseWriter, r *http.Request) {
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	post := Post{}
	err = db.Get(&post, "SELECT `id`, `mime` FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	ext := r.PathValue("ext")

	if ext == "jpg" && post.Mime == "image/jpeg" ||
		ext == "png" && post.Mime == "image/png" ||
		ext == "gif" && post.Mime == "image/gif" {

		// ファイルシステムから画像ファイルを読み込み
		filePath := fmt.Sprintf("../public/image/%d.%s", pid, ext)
		imageData, err := os.ReadFile(filePath)
		if err != nil {
			log.Print(err)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// レスポンスとして返す
		w.Header().Set("Content-Type", post.Mime)
		_, err = w.Write(imageData)
		if err != nil {
			log.Print(err)
			return
		}
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

	// キャッシュを無効化
	memcacheClient.Delete("index_posts")
	// コメントしたユーザーのアカウントページキャッシュも無効化
	cacheKey := fmt.Sprintf("account:%s", me.AccountName)
	memcacheClient.Delete(cacheKey)

	// 投稿者のアカウントページキャッシュも無効化するため、投稿者情報をJOINで一括取得
	var postUserName string
	err = db.Get(&postUserName, "SELECT u.account_name FROM posts p JOIN users u ON p.user_id = u.id WHERE p.id = ?", postID)
	if err == nil {
		postUserCacheKey := fmt.Sprintf("account:%s", postUserName)
		memcacheClient.Delete(postUserCacheKey)
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

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")),
	).Execute(w, struct {
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
		// バンされたユーザーのキャッシュを削除
		cacheKey := fmt.Sprintf("user:%s", id)
		memcacheClient.Delete(cacheKey)
	}

	// キャッシュを無効化（ユーザーがバンされると投稿一覧が変わる可能性がある）
	memcacheClient.Delete("index_posts")

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
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

	log.Fatal(http.ListenAndServe(":8080", r))
}
