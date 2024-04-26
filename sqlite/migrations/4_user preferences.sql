CREATE TABLE user_preferences (
    user_id INTEGER NOT NULL,
    preference_name TEXT NOT NULL,
    preference_value TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, preference_name),
    FOREIGN KEY (user_id) REFERENCES users(id)
);