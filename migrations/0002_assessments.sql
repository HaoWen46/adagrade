-- Assessments, problems, versioned rubrics/reference solutions, roster (spec §4; D4/D5).

-- +goose Up
CREATE TABLE assessments (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('exam', 'assignment')),
    name TEXT NOT NULL,
    archived_at TIMESTAMPTZ, -- soft delete (plan §10)
    created_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Names must be unique among live assessments; archived ones free the name up.
CREATE UNIQUE INDEX assessments_name_active_uniq ON assessments (name) WHERE archived_at IS NULL;

CREATE TABLE problems (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id BIGINT NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    number INT NOT NULL,          -- display + identity within the assessment
    title TEXT NOT NULL DEFAULT '',
    statement TEXT NOT NULL DEFAULT '',
    max_points NUMERIC(6, 2) NOT NULL CHECK (max_points >= 0),
    position INT NOT NULL,        -- ingest page order (spec §7 positional mapping)
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (assessment_id, number),
    UNIQUE (id, assessment_id)    -- composite-FK target so answers can't cross assessments
);

-- Rubric versions are insert-only (D5): editing creates version N+1, records keep
-- referencing the exact version they were graded against.
CREATE TABLE rubric_versions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    problem_id BIGINT NOT NULL REFERENCES problems (id) ON DELETE CASCADE,
    version INT NOT NULL,
    notes TEXT NOT NULL DEFAULT '',
    score_increment NUMERIC(4, 2) NOT NULL DEFAULT 0.5 CHECK (score_increment > 0), -- D4
    created_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (problem_id, version)
);

CREATE TABLE rubric_criteria (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rubric_version_id BIGINT NOT NULL REFERENCES rubric_versions (id) ON DELETE CASCADE,
    position INT NOT NULL,
    description TEXT NOT NULL,
    points NUMERIC(6, 2) NOT NULL CHECK (points >= 0),
    partial_credit_notes TEXT NOT NULL DEFAULT '',
    UNIQUE (rubric_version_id, position)
);

CREATE TABLE reference_solution_versions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    problem_id BIGINT NOT NULL REFERENCES problems (id) ON DELETE CASCADE,
    version INT NOT NULL,
    content TEXT NOT NULL,
    created_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (problem_id, version)
);

-- Roster (D13): student_id is the identity & upsert key.
CREATE TABLE students (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    student_id TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    email TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE students;
DROP TABLE reference_solution_versions;
DROP TABLE rubric_criteria;
DROP TABLE rubric_versions;
DROP TABLE problems;
DROP TABLE assessments;
