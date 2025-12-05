package errors

func OrPanic(err error) {
	if err != nil {
		panic(err)
	}
}
