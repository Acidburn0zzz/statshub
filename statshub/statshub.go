// package statshub implements functionality for submitting and querying stats
// from a centralized stats server.
//
// Stats are always submitted on behalf of a specific user, who is identified by an anonymized integer userid.
// The user is authenticated using Google OAuth, and the userid is checked against the logged-in user by
// comparing a sha-256 hash of the real userid + anonymized userid.  Stats are only stored for anonymized
// user ids.
//
// Example stats updates using curl against a local appengine dev server:
//
//     curl --data-binary '{"countryCode": "ES", "counter": { "mystat": 1, "myotherstat": 50 }, "gauge": {"mygauge": 78, "online": 1}}' "http://localhost:8080/stats/523523?hash=c78c666ec1016b8ed66b40bb46e0883020ff7c9d2f2010c0e2dbfbfc358888a2"
//     curl --data-binary '{"countryCode": "ES", "counter": { "mystat": 2, "myotherstat": 60 }, "gauge": {"mygauge": 55, "online": 1}}' "http://localhost:8080/stats/523524?hash=a3df9bf064bd7e5ca062c4cba9cee839cf5e97cf5c14f5a09a57ca33a719c717"
//
// Example stats get:
//
//     curl -i "http://localhost:8080/stats/523523?hash=c78c666ec1016b8ed66b40bb46e0883020ff7c9d2f2010c0e2dbfbfc358888a2"
//
package statshub

import (
	"appengine"
	"appengine/memcache"
	"appengine/user"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	rollupExpiration = 1 * time.Minute
)

// UserInfo captures the UserId and authentication Hash for a request.
// Requests are authenticated using OAuth and confirmed to be for the
// requested user by matching a sha256 hash of real userid + UserId.
type UserInfo struct {
	UserId int64
	Hash   string // sha256(real userid + userid)
}

// ClientQueryResponse is a Response to a StatsQuery
type ClientQueryResponse struct {
	Response
	User    *Stats           `json:"user"` // Stats for the user
	Rollups *json.RawMessage `json:"rollups"`
}

type CachedRollups struct {
	Global     *Stats            `json:"global"`     // Global stats
	PerCountry map[string]*Stats `json:"perCountry"` // Maps country codes to stats for those countries
}

// Response is a response to a stats request (update or query)
type Response struct {
	Succeeded bool
	Error     string
}

func init() {
	http.HandleFunc("/stats/", statsHandler)
}

// statsPostHandler handles requests to /stats
func statsHandler(w http.ResponseWriter, r *http.Request) {
	userInfo, err := getUserInfo(r)
	if err != nil {
		fail(w, 400, err)
		return
	}

	statusCode, err := userInfo.authenticateAgainst(r)
	if err != nil {
		fail(w, statusCode, err)
		return
	}

	if "POST" == r.Method {
		w.Header().Set("Content-Type", "application/json")

		statusCode, resp, err := postStats(r, userInfo)
		if err != nil {
			fail(w, statusCode, err)
		} else {
			write(w, 200, resp)
		}
	} else if "GET" == r.Method {
		w.Header().Set("Content-Type", "application/json")

		statusCode, resp, err := getStats(r, userInfo)
		if err != nil {
			fail(w, statusCode, err)
		} else {
			write(w, 200, resp)
		}
	} else {
		log.Printf("Query: %s", r.URL.Query())
		w.WriteHeader(405)
	}
}

// postStats handles a POST request to /stats
func postStats(r *http.Request, userInfo *UserInfo) (statusCode int, resp interface{}, err error) {
	decoder := json.NewDecoder(r.Body)
	stats := &StatsUpdate{}
	err = decoder.Decode(stats)
	if err != nil {
		return 400, nil, fmt.Errorf("Unable to decode request: %s", err)
	}

	context := appengine.NewContext(r)
	if err = stats.postToRedis(context, userInfo.UserId); err != nil {
		formattedError := fmt.Errorf("Unable to post stats: %s", err)
		context.Errorf("%s", formattedError)
		return 500, nil, formattedError
	}

	return 200, &Response{Succeeded: true}, nil
}

// getStats handles a GET request to /stats
func getStats(r *http.Request, userInfo *UserInfo) (statusCode int, resp interface{}, err error) {
	context := appengine.NewContext(r)

	clientResp := &ClientQueryResponse{
		Response: Response{Succeeded: true},
	}

	conn, err := connectToRedis(context)
	defer conn.Close()
	if err != nil {
		return 500, nil, fmt.Errorf("Unable to connect to redis: %s", err)
	}

	var cacheItem *memcache.Item
	var calculateRollups = false
	if cacheItem, err = memcache.Get(context, "rollups"); err == memcache.ErrCacheMiss {
		context.Infof("Recomputing rollups")
		calculateRollups = true
	} else if err != nil {
		return
	} else {
		raw := json.RawMessage(cacheItem.Value)
		clientResp.Rollups = &raw
	}

	var queryResp *QueryResponse
	if queryResp, err = query(conn, userInfo.UserId, calculateRollups); err != nil {
		return 500, nil, fmt.Errorf("Unable to query stats: %s", err)
	}
	clientResp.User = queryResp.User
	if calculateRollups {
		rollups := &CachedRollups{
			Global:     queryResp.Global,
			PerCountry: queryResp.PerCountry,
		}
		bytes, _ := json.Marshal(&rollups)
		raw := json.RawMessage(bytes)
		clientResp.Rollups = &raw
		cacheItem = &memcache.Item{
			Key:        "rollups",
			Value:      bytes,
			Expiration: rollupExpiration,
		}
		if cacheErr := memcache.Add(context, cacheItem); cacheErr != nil {
			context.Warningf("Unable to cache rollups: %s", cacheErr)
		}
	}

	return 200, clientResp, nil
}

func getUserInfo(r *http.Request) (userInfo *UserInfo, err error) {
	userInfo = &UserInfo{}

	// Figure out the UserId
	lastSlash := strings.LastIndex(r.URL.Path, "/")
	if lastSlash == 0 {
		return nil, fmt.Errorf("Request URL is missing user id")
	}
	userIdString := r.URL.Path[lastSlash+1:]
	userIdInt, err := strconv.Atoi(userIdString)
	if err != nil {
		return nil, fmt.Errorf("Unable to convert userId %s to int: %s", userIdString, err)
	}
	userInfo.UserId = int64(userIdInt)

	// Figure out the Hash
	hashes, ok := r.URL.Query()["hash"]
	if !ok {
		return nil, fmt.Errorf("No hash provided in querystring")
	}
	if len(hashes) != 1 {
		return nil, fmt.Errorf("Wrong number of hashes provided in querystring")
	}
	userInfo.Hash = hashes[0]

	return
}

// authenticateAgainst compares the Hash in the request with the hash
// calculated based on the currently logged in user.
func (userInfo *UserInfo) authenticateAgainst(r *http.Request) (statusCode int, err error) {
	// Get the currently logged in user
	context := appengine.NewContext(r)
	currentUser, err := user.CurrentOAuth(context, "")
	if err != nil {
		return 401, fmt.Errorf("Not authenticated: %s", err)
	}

	hasher := sha256.New()
	hasher.Reset()
	hashInput := fmt.Sprintf("%s%d", currentUser.Email, userInfo.UserId)
	hasher.Write([]byte(hashInput))
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	if expectedHash != userInfo.Hash {
		return 403, fmt.Errorf("Hash mismatch, authentication failure")
	} else {
		return
	}
}

func fail(w http.ResponseWriter, statusCode int, err error) {
	response := Response{Succeeded: false, Error: fmt.Sprintf("%s", err)}
	write(w, statusCode, response)
}

func write(w http.ResponseWriter, statusCode int, data interface{}) {
	w.WriteHeader(statusCode)
	bytes, err := json.Marshal(data)
	if err == nil {
		w.Write(bytes)
	}
	if err != nil {
		log.Printf("Unable to respond to client: %s", err)
	}
}
