package auth

import "errors"

var (
	
	ErrUserNotFound = errors.New("auth: user not found")

	
	ErrEmailAlreadyExists = errors.New("auth: a user with this email already exists for this tenant")

	
	ErrUserDisabled = errors.New("auth: user is disabled")

	
	ErrInvalidCredentials = errors.New("auth: invalid email or password")

	
	ErrRoleNotFound = errors.New("auth: role not found")

	
	ErrRefreshTokenNotFound = errors.New("auth: refresh token not found")

	
	ErrRefreshTokenInvalid = errors.New("auth: refresh token is not valid")
)