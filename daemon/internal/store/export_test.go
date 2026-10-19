package store

// PublicationReservationKind exposes the store's copy of the publication
// reservation kind to the external test that pins it to the publish
// package's constant, which the store cannot import.
const PublicationReservationKind = publicationReservationKind
