package main

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
)

var (
	clientID     = "your-client-id"
	clientSecret = "your-client-secret"
	redirectURL  = "your-redirect-url"
	oauthConfig  = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.google.com/o/oauth2/auth",
			TokenURL: "https://accounts.google.com/o/oauth2/token",
		},
		Scopes: []string{"profile", "email"},
	}
)

func main() {
	http.HandleFunc("/", handleMain)
	http.HandleFunc("/login", handleLogin)
	http.HandleFunc("/callback", handleCallback)

	fmt.Println("Server is listening on port 8080")
	http.ListenAndServe(":8080", nil)
}

func handleMain(w http.ResponseWriter, r *http.Request) {
	html := `<html><body><a href="/login">Login with Google</a></body></html>`
	w.Write([]byte(html))
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	url := oauthConfig.AuthCodeURL("state", oauth2.AccessTypeOffline)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	token, err := oauthConfig.Exchange(context.Background(), code)
	if err != nil {
		http.Error(w, "Failed to exchange token", http.StatusInternalServerError)
		return
	}

	// You can use the token to make requests to the Google API
	// For example, you might fetch the user's profile information:
	// client := oauthConfig.Client(context.Background(), token)
	// profileInfo, err := client.Get("https://www.googleapis.com/userinfo/v2/me")
	// ...

	fmt.Fprintf(w, "Token: %v\n", token)
}
