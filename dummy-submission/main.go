package main
import (
	"fmt"
	"net/http"
)
func main() {
	http.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Contestant engine accepting orders!")
	})
	fmt.Println("Starting engine on :8080")
	http.ListenAndServe(":8080", nil)
}
