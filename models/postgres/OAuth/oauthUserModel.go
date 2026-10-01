package models

// User model for db
type User struct {
	Key string `json:"_key,omitempty" bson:"_key,omitempty" cql:"_key,omitempty" dynamo:"key,omitempty"` // for arangodb
	ID  string `gorm:"primaryKey;type:char(36)" json:"_id" bson:"_id" cql:"id" dynamo:"id,hash"`

	Email                    *string `gorm:"unique" json:"email" bson:"email" cql:"email" dynamo:"email" index:"email,hash"`
	GithubName               *string `json:"name,omitempty"`
	EmailVerifiedAt          *int64  `json:"email_verified_at" bson:"email_verified_at" cql:"email_verified_at" dynamo:"email_verified_at"`
	Password                 *string `json:"password" bson:"password" cql:"password" dynamo:"password"`
	SignupMethods            string  `json:"signup_methods" bson:"signup_methods" cql:"signup_methods" dynamo:"signup_methods"`
	GivenName                *string `json:"given_name" bson:"given_name" cql:"given_name" dynamo:"given_name"`
	FamilyName               *string `json:"family_name" bson:"family_name" cql:"family_name" dynamo:"family_name"`
	MiddleName               *string `json:"middle_name" bson:"middle_name" cql:"middle_name" dynamo:"middle_name"`
	Nickname                 *string `json:"nickname" bson:"nickname" cql:"nickname" dynamo:"nickname"`
	Gender                   *string `json:"gender" bson:"gender" cql:"gender" dynamo:"gender"`
	Birthdate                *string `json:"birthdate" bson:"birthdate" cql:"birthdate" dynamo:"birthdate"`
	PhoneNumber              *string `gorm:"index" json:"phone_number" bson:"phone_number" cql:"phone_number" dynamo:"phone_number"`
	PhoneNumberVerifiedAt    *int64  `json:"phone_number_verified_at" bson:"phone_number_verified_at" cql:"phone_number_verified_at" dynamo:"phone_number_verified_at"`
	Picture                  *string `json:"picture" bson:"picture" cql:"picture" dynamo:"picture"`
	Roles                    string  `json:"roles" bson:"roles" cql:"roles" dynamo:"roles"`
	RevokedTimestamp         *int64  `json:"revoked_timestamp" bson:"revoked_timestamp" cql:"revoked_timestamp" dynamo:"revoked_timestamp"`
	IsMultiFactorAuthEnabled *bool   `json:"is_multi_factor_auth_enabled" bson:"is_multi_factor_auth_enabled" cql:"is_multi_factor_auth_enabled" dynamo:"is_multi_factor_auth_enabled"`
	AppData                  *string `json:"app_data" bson:"app_data" cql:"app_data" dynamo:"app_data"`
}
