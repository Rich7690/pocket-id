package service

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	profilepicture "github.com/pocket-id/pocket-id/backend/internal/utils/image"

	"github.com/pocket-id/pocket-id/backend/internal/common"
	"github.com/pocket-id/pocket-id/backend/internal/dto"
	"github.com/pocket-id/pocket-id/backend/internal/model"
	datatype "github.com/pocket-id/pocket-id/backend/internal/model/types"
	"github.com/pocket-id/pocket-id/backend/internal/utils"
	"github.com/pocket-id/pocket-id/backend/internal/utils/email"
	"gorm.io/gorm"
)

type UserService struct {
	db               *gorm.DB
	jwtService       *JwtService
	auditLogService  *AuditLogService
	emailService     *EmailService
	appConfigService *AppConfigService
}

func NewUserService(db *gorm.DB, jwtService *JwtService, auditLogService *AuditLogService, emailService *EmailService, appConfigService *AppConfigService) *UserService {
	return &UserService{db: db, jwtService: jwtService, auditLogService: auditLogService, emailService: emailService, appConfigService: appConfigService}
}

func (s *UserService) ListUsers(searchTerm string, sortedPaginationRequest utils.SortedPaginationRequest) ([]model.User, utils.PaginationResponse, error) {
	var users []model.User
	query := s.db.Model(&model.User{})

	if searchTerm != "" {
		searchPattern := "%" + searchTerm + "%"
		query = query.Where("email LIKE ? OR first_name LIKE ? OR username LIKE ?", searchPattern, searchPattern, searchPattern)
	}

	pagination, err := utils.PaginateAndSort(sortedPaginationRequest, query, &users)
	return users, pagination, err
}

func (s *UserService) GetUser(userID string) (model.User, error) {
	var user model.User
	err := s.db.Preload("UserGroups").Preload("CustomClaims").Where("id = ?", userID).First(&user).Error
	return user, err
}

func (s *UserService) GetProfilePicture(userID string) (io.Reader, int64, error) {
	// Validate the user ID to prevent directory traversal
	if err := uuid.Validate(userID); err != nil {
		return nil, 0, &common.InvalidUUIDError{}
	}

	profilePicturePath := common.EnvConfig.UploadPath + "/profile-pictures/" + userID + ".png"
	file, err := os.Open(profilePicturePath)
	if err == nil {
		// Get the file size
		fileInfo, err := file.Stat()
		if err != nil {
			return nil, 0, err
		}
		return file, fileInfo.Size(), nil
	}

	// If the file does not exist, return the default profile picture
	user, err := s.GetUser(userID)
	if err != nil {
		return nil, 0, err
	}

	defaultPicture, err := profilepicture.CreateDefaultProfilePicture(user.FirstName, user.LastName)
	if err != nil {
		return nil, 0, err
	}

	return defaultPicture, int64(defaultPicture.Len()), nil
}

func (s *UserService) GetUserGroups(userID string) ([]model.UserGroup, error) {
	var user model.User
	if err := s.db.Preload("UserGroups").Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, err
	}
	return user.UserGroups, nil
}

func (s *UserService) UpdateProfilePicture(userID string, file io.Reader) error {
	// Validate the user ID to prevent directory traversal
	err := uuid.Validate(userID)
	if err != nil {
		return &common.InvalidUUIDError{}
	}

	// Convert the image to a smaller square image
	profilePicture, err := profilepicture.CreateProfilePicture(file)
	if err != nil {
		return err
	}

	// Ensure the directory exists
	profilePictureDir := common.EnvConfig.UploadPath + "/profile-pictures"
	err = os.MkdirAll(profilePictureDir, os.ModePerm)
	if err != nil {
		return err
	}

	// Create the profile picture file
	err = utils.SaveFileStream(profilePicture, profilePictureDir+"/"+userID+".png")
	if err != nil {
		return err
	}

	return nil
}

func (s *UserService) DeleteUser(userID string) error {
	var user model.User
	if err := s.db.Where("id = ?", userID).First(&user).Error; err != nil {
		return err
	}

	// Disallow deleting the user if it is an LDAP user and LDAP is enabled
	if user.LdapID != nil && s.appConfigService.DbConfig.LdapEnabled.IsTrue() {
		return &common.LdapUserUpdateError{}
	}

	// Delete the profile picture
	profilePicturePath := common.EnvConfig.UploadPath + "/profile-pictures/" + userID + ".png"
	if err := os.Remove(profilePicturePath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return s.db.Delete(&user).Error
}

func (s *UserService) CreateUser(input dto.UserCreateDto) (model.User, error) {
	user := model.User{
		FirstName: input.FirstName,
		LastName:  input.LastName,
		Email:     input.Email,
		Username:  input.Username,
		IsAdmin:   input.IsAdmin,
		Locale:    input.Locale,
	}
	if input.LdapID != "" {
		user.LdapID = &input.LdapID
	}

	if err := s.db.Create(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return model.User{}, s.checkDuplicatedFields(user)
		}
		return model.User{}, err
	}
	return user, nil
}

func (s *UserService) UpdateUser(userID string, updatedUser dto.UserCreateDto, updateOwnUser bool, allowLdapUpdate bool) (model.User, error) {
	var user model.User
	if err := s.db.Where("id = ?", userID).First(&user).Error; err != nil {
		return model.User{}, err
	}

	// Disallow updating the user if it is an LDAP group and LDAP is enabled
	if !allowLdapUpdate && user.LdapID != nil && s.appConfigService.DbConfig.LdapEnabled.IsTrue() {
		return model.User{}, &common.LdapUserUpdateError{}
	}

	user.FirstName = updatedUser.FirstName
	user.LastName = updatedUser.LastName
	user.Email = updatedUser.Email
	user.Username = updatedUser.Username
	user.Locale = updatedUser.Locale
	if !updateOwnUser {
		user.IsAdmin = updatedUser.IsAdmin
	}

	if err := s.db.Save(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return user, s.checkDuplicatedFields(user)
		}
		return user, err
	}

	return user, nil
}

func (s *UserService) RequestOneTimeAccessEmail(emailAddress, redirectPath string) error {
	isDisabled := !s.appConfigService.DbConfig.EmailOneTimeAccessEnabled.IsTrue()
	if isDisabled {
		return &common.OneTimeAccessDisabledError{}
	}

	var user model.User
	if err := s.db.Where("email = ?", emailAddress).First(&user).Error; err != nil {
		// Do not return error if user not found to prevent email enumeration
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		} else {
			return err
		}
	}

	oneTimeAccessToken, err := s.CreateOneTimeAccessToken(user.ID, time.Now().Add(15*time.Minute))
	if err != nil {
		return err
	}

	link := fmt.Sprintf("%s/lc", common.EnvConfig.AppURL)
	linkWithCode := fmt.Sprintf("%s/%s", link, oneTimeAccessToken)

	// Add redirect path to the link
	if strings.HasPrefix(redirectPath, "/") {
		encodedRedirectPath := url.QueryEscape(redirectPath)
		linkWithCode = fmt.Sprintf("%s?redirect=%s", linkWithCode, encodedRedirectPath)
	}

	go func() {
		err := SendEmail(s.emailService, email.Address{
			Name:  user.Username,
			Email: user.Email,
		}, OneTimeAccessTemplate, &OneTimeAccessTemplateData{
			Code:              oneTimeAccessToken,
			LoginLink:         link,
			LoginLinkWithCode: linkWithCode,
		})
		if err != nil {
			log.Printf("Failed to send email to '%s': %v\n", user.Email, err)
		}
	}()

	return nil
}

func (s *UserService) CreateOneTimeAccessToken(userID string, expiresAt time.Time) (string, error) {
	tokenLength := 16

	// If expires at is less than 15 minutes, use an 6 character token instead of 16
	if time.Until(expiresAt) <= 15*time.Minute {
		tokenLength = 6
	}

	randomString, err := utils.GenerateRandomAlphanumericString(tokenLength)
	if err != nil {
		return "", err
	}

	oneTimeAccessToken := model.OneTimeAccessToken{
		UserID:    userID,
		ExpiresAt: datatype.DateTime(expiresAt),
		Token:     randomString,
	}

	if err := s.db.Create(&oneTimeAccessToken).Error; err != nil {
		return "", err
	}

	return oneTimeAccessToken.Token, nil
}

func (s *UserService) ExchangeOneTimeAccessToken(token string, ipAddress, userAgent string) (model.User, string, error) {
	var oneTimeAccessToken model.OneTimeAccessToken
	if err := s.db.Where("token = ? AND expires_at > ?", token, datatype.DateTime(time.Now())).Preload("User").First(&oneTimeAccessToken).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.User{}, "", &common.TokenInvalidOrExpiredError{}
		}
		return model.User{}, "", err
	}
	accessToken, err := s.jwtService.GenerateAccessToken(oneTimeAccessToken.User)
	if err != nil {
		return model.User{}, "", err
	}

	if err := s.db.Delete(&oneTimeAccessToken).Error; err != nil {
		return model.User{}, "", err
	}

	if ipAddress != "" && userAgent != "" {
		s.auditLogService.Create(model.AuditLogEventOneTimeAccessTokenSignIn, ipAddress, userAgent, oneTimeAccessToken.User.ID, model.AuditLogData{})
	}

	return oneTimeAccessToken.User, accessToken, nil
}

func (s *UserService) UpdateUserGroups(id string, userGroupIds []string) (user model.User, err error) {
	user, err = s.GetUser(id)
	if err != nil {
		return model.User{}, err
	}

	// Fetch the groups based on userGroupIds
	var groups []model.UserGroup
	if len(userGroupIds) > 0 {
		if err := s.db.Where("id IN (?)", userGroupIds).Find(&groups).Error; err != nil {
			return model.User{}, err
		}
	}

	// Replace the current groups with the new set of groups
	if err := s.db.Model(&user).Association("UserGroups").Replace(groups); err != nil {
		return model.User{}, err
	}

	// Save the updated user
	if err := s.db.Save(&user).Error; err != nil {
		return model.User{}, err
	}

	return user, nil
}

func (s *UserService) SetupInitialAdmin() (model.User, string, error) {
	var userCount int64
	if err := s.db.Model(&model.User{}).Count(&userCount).Error; err != nil {
		return model.User{}, "", err
	}
	if userCount > 1 {
		return model.User{}, "", &common.SetupAlreadyCompletedError{}
	}

	user := model.User{
		FirstName: "Admin",
		LastName:  "Admin",
		Username:  "admin",
		Email:     "admin@admin.com",
		IsAdmin:   true,
	}

	if err := s.db.Model(&model.User{}).Preload("Credentials").FirstOrCreate(&user).Error; err != nil {
		return model.User{}, "", err
	}

	if len(user.Credentials) > 0 {
		return model.User{}, "", &common.SetupAlreadyCompletedError{}
	}

	token, err := s.jwtService.GenerateAccessToken(user)
	if err != nil {
		return model.User{}, "", err
	}

	return user, token, nil
}

func (s *UserService) checkDuplicatedFields(user model.User) error {
	var existingUser model.User
	if s.db.Where("id != ? AND email = ?", user.ID, user.Email).First(&existingUser).Error == nil {
		return &common.AlreadyInUseError{Property: "email"}
	}

	if s.db.Where("id != ? AND username = ?", user.ID, user.Username).First(&existingUser).Error == nil {
		return &common.AlreadyInUseError{Property: "username"}
	}

	return nil
}

// ResetProfilePicture deletes a user's custom profile picture
func (s *UserService) ResetProfilePicture(userID string) error {
	// Validate the user ID to prevent directory traversal
	if err := uuid.Validate(userID); err != nil {
		return &common.InvalidUUIDError{}
	}

	// Build path to profile picture
	profilePicturePath := common.EnvConfig.UploadPath + "/profile-pictures/" + userID + ".png"

	// Check if file exists and delete it
	if _, err := os.Stat(profilePicturePath); err == nil {
		if err := os.Remove(profilePicturePath); err != nil {
			return fmt.Errorf("failed to delete profile picture: %w", err)
		}
	} else if !os.IsNotExist(err) {
		// If any error other than "file not exists"
		return fmt.Errorf("failed to check if profile picture exists: %w", err)
	}
	// It's okay if the file doesn't exist - just means there's no custom picture to delete

	return nil
}
