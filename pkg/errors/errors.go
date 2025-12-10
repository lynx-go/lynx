package errors

import (
	"fmt"
	"log"
)

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e Error) Error() string {
	return fmt.Sprintf("%d: %s", e.Code, e.Message)
}

var _ error = new(Error)

func Fatal(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
