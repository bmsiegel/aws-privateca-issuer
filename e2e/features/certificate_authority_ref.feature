@CertificateAuthorityRef
Feature: Issue certificates using certificateAuthorityRef
  As a user of the aws-privateca-issuer with ACK
  I need to be able to reference an ACK CertificateAuthority by name
  Instead of providing a hardcoded ARN

  @ACK
  Scenario: Issue a certificate with a ClusterIssuer using certificateAuthorityRef
    Given I create an ACK CertificateAuthority resource
    And I create an AWSPCAClusterIssuer using a certificateAuthorityRef
    When I issue a RSA certificate
    Then the certificate should be issued successfully

  @ACK
  Scenario: ClusterIssuer becomes ready after ACK CA is activated
    Given I create an AWSPCAClusterIssuer referencing a non-existent CA
    Then the issuer should have status Waiting
    When I create an ACK CertificateAuthority resource with the referenced name
    Then the issuer should become ready within 60 seconds

  @ACK
  Scenario: Issue a certificate with a namespaced Issuer using certificateAuthorityRef
    Given I create a namespace
    And I create an ACK CertificateAuthority resource in the namespace
    And I create an AWSPCAIssuer using a certificateAuthorityRef
    When I issue a RSA certificate
    Then the certificate should be issued successfully
