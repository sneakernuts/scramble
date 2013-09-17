package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"
	"net/url"
)

//
// UTILITY FUNCTIONS
//

func groupAddrsByHost(addrs []string) map[string][]Address {
	hostAddrs := map[string][]Address{}
	for _, addr := range addrs {
		addr = validateAddress(addr)
		match := regexAddress.FindStringSubmatch(addr)
		name := match[1]
		host := match[2]
		hostAddrs[host] = append(hostAddrs[host], Address{name, host})
	}
	return hostAddrs
}

//
// SERVE HTML, CSS, JS
//

func staticHandler(w http.ResponseWriter, r *http.Request) {
	var path string
	if strings.HasSuffix(r.URL.Path, "/") {
		path = r.URL.Path + "index.html"
	} else {
		path = r.URL.Path
	}
	http.ServeFile(w, r, "static/"+path)
}

//
// USER ROUTE
//

func userHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		publicKeyHandler(w, r)
	} else if r.Method == "POST" {
		createHandler(w, r)
	}
}

// GET /user/<public key hash> for public key lookup
// The server is untrusted, so the client will verify in Javascript
// that the public key we send here matches the hash they requested
func publicKeyHandler(w http.ResponseWriter, r *http.Request) {
	userPubHash := validateHash(r.URL.Path[len("/user/"):])
	userPub := LoadPubKey(userPubHash)
	if userPub == "" {
		http.Error(w, "Not found", http.StatusNotFound)
	} else {
		w.Write([]byte(userPub))
	}
}

// POST /publickeys to look up many public keys from
//  many <public key hash>@<host> addresses.
// The server is untrusted, so the client must verify by hashing.
// Unknown public key hashes cause the server to dispatch requests
//  to the address host.
func publicKeysHandler( w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)

	// parse addresses & group by host
	addrs := r.FormValue("addresses")
	hostAddrs := groupAddrsByHost(strings.Split(addrs, ","))

	// res will get returned as json: {address: {pubkey, err}}
	type PubKeyErr struct {
		PubKey string `json:"pubKey,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	res := map[string]*PubKeyErr{}

	if userId == nil {
		// server-to-server requests need no userId,
		// but all requested addresses should belong to this server.
		// all lookups will be done locally rather than dispatching further.
		for _, addrs := range hostAddrs {
			for _, addr := range addrs {
				if !addr.IsHashAddress() {
					panic("publicKeysHandler can't handle non-hash-addresses")
				}
				pubKey := LoadPubKey(addr.Name)
				if pubKey == "" {
					res[addr.String()] = &PubKeyErr{"", "Unknown address"}
				} else {
					res[addr.String()] = &PubKeyErr{pubKey, ""}
				}
			}
		}
		resJson, err := json.Marshal(res)
		if err != nil { panic(err) }
		w.Write(resJson)
	} else {
		// dispatch requests to each server.
		// TODO could be smarter about which host(s) is local.
		type HostRespErr struct {
			Host string
			Resp *http.Response
			Err  error
		}
		ch := make(chan *HostRespErr)
		for host, addrs := range hostAddrs {
			go func() {
				u := url.URL{}
				u.Scheme = "https"
				u.Host = host
				u.Path = "/publickeys/"
				body := url.Values{}
				addrStrs := []string{}
				for _, addr := range addrs {
					addrStrs = append(addrStrs, addr.String())
				}
				body.Set("addresses", strings.Join(addrStrs, ","))
				resp, err := http.PostForm(u.String(), body)
				ch <- &HostRespErr{host, resp, err}
			}()
		}
		// update `res` with responses
		counter := len(hostAddrs)
		timeout := time.After(5 * time.Second)
		timedOut := false
		for counter > 0 && !timedOut {
			select {
				case hostRespErr := <-ch:
					counter -= 1
					if hostRespErr.Err != nil { continue } // TODO better error messages
					respBody, err := ioutil.ReadAll(hostRespErr.Resp.Body)
					defer hostRespErr.Resp.Body.Close()
					if err != nil { continue } // TODO better error messages
					parsed := map[string]*PubKeyErr{}
					err = json.Unmarshal(respBody, &parsed)
					if err != nil { continue } // TODO better error messages
					for addr, pubKeyErr := range parsed {
						res[addr] = pubKeyErr
					}
				case <-timeout:
					timedOut = true
			}
		}
		// fill remaining addresses with appropriate error messages
		// client must still verify that addresses aren't missing
		for _, addrs := range hostAddrs {
			for _, addr := range addrs {
				if res[addr.String()] == nil {
					res[addr.String()] = &PubKeyErr{"", "Failed to retrieve public key"}
				}
			}
		}
		// respond back
		resJson, err := json.Marshal(res)
		if err != nil { panic(err) }
		w.Write(resJson)
		// drain ch
		for counter > 0 {
			select {
				case hostRespErr := <-ch:
					counter -= 1
					if hostRespErr.Err != nil { continue }
					hostRespErr.Resp.Body.Close()
			}
		}
	}
}

// POST /user to create a new account
// Remember that public and private key generation happens
// on the client. Public key, encrypted private key posted here.
func createHandler(w http.ResponseWriter, r *http.Request) {
	user := new(User)
	user.Token = validateToken(r.FormValue("token"))
	user.PasswordHash = validatePassHash(r.FormValue("passHash"))
	user.PublicKey = validatePublicKey(r.FormValue("publicKey"))
	user.PublicHash = computePublicHash(user.PublicKey)
	user.CipherPrivateKey = validateHex(r.FormValue("cipherPrivateKey"))

	log.Printf("Woot! New user %s %s\n", user.Token, user.PublicHash)

	if !SaveUser(user) {
		http.Error(w, "That username is taken", http.StatusBadRequest)
	}
}

// GET /user/me/contacts for the logged-in user's encrypted address book
// POST /user/me/contacts to update logged-in user's encrypted address book
// The entire address book is a single blob.
// Because the server never knows the plaintext, it is also
// unable to update individual keys in address book -- whenever
// the user makes changes, the client encrypts and posts all contacts
func contactsHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)

	if r.Method == "GET" {
		cipherContactsHex := LoadContacts(userId.Token)
		if cipherContactsHex == nil {
			http.Error(w, "Not found", http.StatusNotFound)
		} else {
			w.Write([]byte(*cipherContactsHex))
		}
	} else if r.Method == "POST" {
		cipherContactsHex, err := ioutil.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		SaveContacts(userId.Token, string(cipherContactsHex))
	}
}

// GET /user/me/key for the logged-in user's encrypted private key
func privateKeyHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)

	user := LoadUser(userId.Token)
	if user == nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	w.Write([]byte(user.CipherPrivateKey))
}

//
// AUTHENTICATION
//

// Checks cookies, returns the logged-in user token or empty string
func authenticate(r *http.Request) *UserID {
	token, err := r.Cookie("token")
	if err != nil {
		return nil
	}
	passHash, err := r.Cookie("passHash")
	if err != nil {
		return nil
	}
	passHashOld, err := r.Cookie("passHashOld")
	var passHashOldVal string
	if err != nil {
		passHashOldVal = ""
	} else {
		passHashOldVal = passHashOld.Value
	}
	userId := authenticateUserPass(token.Value, passHash.Value, passHashOldVal)

	// TODO: user email address should be stored, not computed
	if r.Host == "localhost" || strings.HasPrefix(r.Host, "localhost:") {
		userId.EmailAddress = userId.PublicHash + "@scramble.io"
	} else {
		userId.EmailAddress = userId.PublicHash + "@" + r.Host
	}
	return userId
}

func authenticateUserPass(token string, passHash string, passHashOld string) *UserID {
	// look up the user
	userId := LoadUserID(token)
	if userId == nil {
		return nil
	}

	// verify password
	if passHash == userId.PasswordHash && passHash != "" {
		return userId
	}
	if passHashOld == userId.PasswordHashOld && passHashOld != "" {
		return userId
	}
	return nil
}

//
// INBOX ROUTE
//

// Takes no arguments, returns all the metadata about a user's inbox.
// Encrypted subjects are returned, but no message bodies.
// The caller must have auth cookies set.
func inboxHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)
	if userId == nil {
		http.Error(w, "Not logged in", http.StatusUnauthorized)
		return
	}
	box := r.URL.Path[len("/box/"):]

	var emailHeaders []EmailHeader
	if box == "inbox" || box == "archive" || box == "sent" {
		emailHeaders = LoadBox(userId.EmailAddress, box)
	} else {
		http.Error(w, "Unknown box. "+
			"Expected 'inbox','sent', etc, got "+box,
			http.StatusBadRequest)
		return
	}

	var inbox InboxSummary
	inbox.Token = userId.Token
	inbox.PublicHash = userId.PublicHash
	inbox.EmailHeaders = emailHeaders

	inboxJson, err := json.Marshal(inbox)
	if err != nil {
		panic(err)
	}
	w.Write(inboxJson)
}

//
// EMAIL ROUTE
//

func emailHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		emailFetchHandler(w, r)
	} else if r.Method == "PUT" {
		emailBoxHandler(w, r)
	} else if r.Method == "POST" {
		emailSendHandler(w, r)
	}
}

// GET /email/id fetches the body
func emailFetchHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)
	if userId == nil {
		http.Error(w, "Not logged in", http.StatusUnauthorized)
		return
	}

	id := r.URL.Path[len("/email/"):]
	validateMessageID(id)

	message := LoadMessage(id)
	// XXX ensure that message belongs to user
	w.Write([]byte(message.CipherBody))
}

// PUT /email/id can change things about an email, eg what box it's in
func emailBoxHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)
	if userId == nil {
		http.Error(w, "Not logged in", http.StatusUnauthorized)
		return
	}

	id := r.URL.Path[len("/email/"):]
	validateMessageID(id)
	newBox := validateBox(r.FormValue("box"))

	UpdateEmail(userId.EmailAddress, id, newBox)
}

func emailSendHandler(w http.ResponseWriter, r *http.Request) {
	userId := authenticate(r)
	if userId == nil {
		http.Error(w, "Not logged in", http.StatusUnauthorized)
		return
	}

	email := new(Email)
	email.MessageID = validateMessageID(r.FormValue("msgId"))
	email.UnixTime = time.Now().Unix()
	email.From = userId.EmailAddress
	email.To = r.FormValue("to")

	// XXX remove this case
	if r.FormValue("cipherBody") == "" { // unencrypted
		email.CipherSubject = r.FormValue("subject")
		email.CipherBody = r.FormValue("body")
	} else { // encrypted
		email.CipherSubject = validateHex(r.FormValue("cipherSubject"))
		email.CipherBody = validateHex(r.FormValue("cipherBody"))
	}

	// TODO: consider if transactions are required.
	// TODO: saveMessage may fail if messageId is not unique.
	SaveMessage(email)

	// add message to sender's sent box
	AddMessageToBox(email, userId.EmailAddress, "sent")

	// TODO: separate goroutine?
	// TODO: parallize mx lookup?
	// for each address, lookup MX record & determine what to do.
	hostAddrs := groupAddrsByHost(strings.Split(email.To, ","))
	for host, addrs := range hostAddrs {
		server, err := smtpLookUp(host)
		if err != nil {
			panic(err) // TODO: what to do?
		}
		if server == GetConfig().ThisMxHost {
			// add to inbox locally
			for _, addr := range addrs {
				AddMessageToBox(email, addr.String(), "inbox")
			}
		} else {
			// add to outbox for delivery
			// note that we only store the host for the 'address' column,
			// and only once for multiple recipients on the same host.
			AddMessageToBox(email, host, "outbox")
		}
	}
}
